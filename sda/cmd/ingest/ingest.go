// The ingest service accepts messages for files uploaded to the inbox,
// registers the files in the database with their headers, and stores them
// header-stripped in the archive storage.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/neicnordic/crypt4gh/model/headers"
	"github.com/neicnordic/crypt4gh/streaming"
	"github.com/neicnordic/sensitive-data-archive/internal/broker"
	"github.com/neicnordic/sensitive-data-archive/internal/config"
	"github.com/neicnordic/sensitive-data-archive/internal/database"
	"github.com/neicnordic/sensitive-data-archive/internal/schema"
	"github.com/neicnordic/sensitive-data-archive/internal/storage"

	log "github.com/sirupsen/logrus"
)

func main() {
	sigc := make(chan os.Signal, 5)
	signal.Notify(sigc, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	// Create a function to handle panic and exit gracefully
	defer func() {
		if err := recover(); err != nil {
			log.Fatal("Could not recover, exiting")
		}
	}()

	forever := make(chan bool)
	conf, err := config.NewConfig("ingest")
	if err != nil {
		log.Error(err)
		sigc <- syscall.SIGINT
		panic(err)
	}
	mq, err := broker.NewMQ(conf.Broker)
	if err != nil {
		log.Error(err)
		sigc <- syscall.SIGINT
		panic(err)
	}
	db, err := database.NewSDAdb(conf.Database)
	if err != nil {
		log.Error(err)
		sigc <- syscall.SIGINT
		panic(err)
	}
	if db.Version < 8 {
		log.Error("database schema v8 is required")
		sigc <- syscall.SIGINT
		panic(err)
	}
	key, err := config.GetC4GHKey()
	if err != nil {
		log.Error(err)
		sigc <- syscall.SIGINT
		panic(err)
	}
	archive, err := storage.NewBackend(conf.Archive)
	if err != nil {
		log.Error(err)
		sigc <- syscall.SIGINT
		panic(err)
	}
	inbox, err := storage.NewBackend(conf.Inbox)
	if err != nil {
		log.Error(err)
		sigc <- syscall.SIGINT
		panic(err)
	}

	defer mq.Channel.Close()
	defer mq.Connection.Close()
	defer db.Close()

	go func() {
		connError := mq.ConnectionWatcher()
		log.Error(connError)
		forever <- false
	}()

	go func() {
		connError := mq.ChannelWatcher()
		log.Error(connError)
		forever <- false
	}()

	log.Info("starting ingest service")
	var message schema.IngestionTrigger

	go func() {
		messages, err := mq.GetMessages(conf.Broker.Queue)
		if err != nil {
			log.Fatal(err)
		}
	mainWorkLoop:
		for delivered := range messages {
			log.Debugf("received a message (corr-id: %s, message: %s)", delivered.CorrelationId, delivered.Body)
			err := schema.ValidateJSON(fmt.Sprintf("%s/ingestion-trigger.json", conf.Broker.SchemasPath), delivered.Body)
			if err != nil {
				log.Errorf("validation of incoming message (ingestion-trigger) failed, reason: %v", err.Error())
				// Send the message to an error queue so it can be analyzed.
				infoErrorMessage := broker.InfoError{
					Error:           "Message validation failed",
					Reason:          err.Error(),
					OriginalMessage: message,
				}

				body, _ := json.Marshal(infoErrorMessage)
				if e := mq.SendMessage(delivered.CorrelationId, conf.Broker.Exchange, "error", body); e != nil {
					log.Errorf("failed so publish message, reason: (%v)", err.Error())
				}
				if err := delivered.Ack(false); err != nil {
					log.Errorf("Failed acking canceled work, reason: (%v)", err.Error())
				}

				// Restart on new message
				continue
			}

			// we unmarshal the message in the validation step so this is safe to do
			_ = json.Unmarshal(delivered.Body, &message)

			log.Infof(
				"Received work (corr-id: %s, filepath: %s, user: %s)",
				delivered.CorrelationId, message.FilePath, message.User,
			)

			switch message.Type {
			case "cancel":
				fileUUID, err := db.GetFileID(delivered.CorrelationId)
				if err != nil || fileUUID == "" {
					log.Errorf("failed to get ID for file from message: %v", delivered.CorrelationId)

					if err = delivered.Nack(false, false); err != nil {
						log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
					}

					continue
				}

				if err := db.UpdateFileStatus(fileUUID, "disabled", delivered.CorrelationId, message.User, string(delivered.Body)); err != nil {
					log.Errorf("failed to set ingestion status for file from message: %v", delivered.CorrelationId)
					if err = delivered.Nack(false, false); err != nil {
						log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
					}

					continue
				}

				if err := delivered.Ack(false); err != nil {
					log.Errorf("failed to ack message for reason: (%v)", err.Error())
				}

				continue
			case "ingest":
				file, err := inbox.NewFileReader(message.FilePath)
				if err != nil {
					log.Errorf("Failed to open file to ingest reason: (%v)", err.Error())
					// Send the message to an error queue so it can be analyzed.
					fileError := broker.InfoError{
						Error:           "Failed to open file to ingest",
						Reason:          err.Error(),
						OriginalMessage: message,
					}
					body, _ := json.Marshal(fileError)
					if e := mq.SendMessage(delivered.CorrelationId, conf.Broker.Exchange, "error", body); e != nil {
						log.Errorf("failed so publish message, reason: (%v)", err.Error())
					}
					if err = delivered.Ack(false); err != nil {
						log.Errorf("Failed to Ack message, reason: (%v)", err.Error())
					}

					// Restart on new message
					continue
				}

				fileSize, err := inbox.GetFileSize(message.FilePath)
				if err != nil {
					log.Errorf("Failed to get file size of file to ingest, reason: %v", err.Error())
					// Nack message so the server gets notified that something is wrong and requeue the message.
					// Since reading the file worked, this should eventually succeed so it is ok to requeue.
					if err = delivered.Nack(false, true); err != nil {
						log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
					}
					// Send the message to an error queue so it can be analyzed.
					fileError := broker.InfoError{
						Error:           "Failed to get file size of file to ingest",
						Reason:          err.Error(),
						OriginalMessage: message,
					}
					body, _ := json.Marshal(fileError)
					if err = mq.SendMessage(delivered.CorrelationId, conf.Broker.Exchange, "error", body); err != nil {
						log.Errorf("failed so publish message, reason: (%v)", err.Error())
					}

					// Restart on new message
					continue
				}

				fileID, err := db.RegisterFile(message.FilePath, message.User)
				if err != nil {
					log.Errorf("InsertFile failed, reason: (%v)", err.Error())
				}
				err = db.UpdateFileStatus(fileID, "submitted", delivered.CorrelationId, message.User, string(delivered.Body))
				if err != nil {
					log.Errorf("failed to set ingestion status for file from message: %v", delivered.CorrelationId)
				}

				dest, err := archive.NewFileWriter(fileID)
				if err != nil {
					log.Errorf("Failed to create archive file, reason: (%v)", err.Error())
					// Nack message so the server gets notified that something is wrong and requeue the message.
					// NewFileWriter returns an error when the backend itself fails so this is reasonable to requeue.
					if err = delivered.Nack(false, true); err != nil {
						log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
					}

					continue
				}

				// 4MiB readbuffer, this must be large enough that we get the entire header and the first 64KiB datablock
				var bufSize int
				if bufSize = 4 * 1024 * 1024; conf.Inbox.S3.Chunksize > 4*1024*1024 {
					bufSize = conf.Inbox.S3.Chunksize
				}
				readBuffer := make([]byte, bufSize)
				hash := sha256.New()
				var bytesRead int64
				var byteBuf bytes.Buffer

				for bytesRead < fileSize {
					i, _ := io.ReadFull(file, readBuffer)
					if i == 0 {
						return
					}
					// truncate the readbuffer if the file is smaller than the buffer size
					if i < len(readBuffer) {
						readBuffer = readBuffer[:i]
					}

					bytesRead += int64(i)

					h := bytes.NewReader(readBuffer)
					if _, err = io.Copy(hash, h); err != nil {
						log.Errorf("Copy to hash failed while reading file, reason: (%v)", err.Error())
						if err = delivered.Nack(false, true); err != nil {
							log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
						}

						continue mainWorkLoop
					}

					//nolint:nestif
					if bytesRead <= int64(len(readBuffer)) {
						header, err := tryDecrypt(key, readBuffer)
						if err != nil {
							log.Errorf("Trying to decrypt start of file failed, reason: (%v)", err.Error())

							// Nack message so the server gets notified that something is wrong. Do not requeue the message.
							if err = delivered.Nack(false, false); err != nil {
								log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
							}

							// Send the message to an error queue so it can be analyzed.
							fileError := broker.InfoError{
								Error:           "Trying to decrypt start of file failed",
								Reason:          err.Error(),
								OriginalMessage: message,
							}
							body, _ := json.Marshal(fileError)
							if e := mq.SendMessage(delivered.CorrelationId, conf.Broker.Exchange, "error", body); e != nil {
								log.Errorf("failed so publish message, reason: (%v)", err.Error())
							}

							continue mainWorkLoop
						}

						log.Debugln("store header")
						if err := db.StoreHeader(header, fileID); err != nil {
							log.Errorf("StoreHeader failed, reason: (%v)", err.Error())
							if err = delivered.Nack(false, true); err != nil {
								log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
							}

							continue mainWorkLoop
						}

						if _, err = byteBuf.Write(readBuffer); err != nil {
							log.Errorf("Failed to write to read buffer for header read, reason: %v)", err.Error())
							if err = delivered.Nack(false, true); err != nil {
								log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
							}

							continue mainWorkLoop
						}

						// Strip header from buffer
						h := make([]byte, len(header))
						if _, err = byteBuf.Read(h); err != nil {
							log.Errorf("Failed to strip header from buffer, reason: (%v)", err.Error())
							if err = delivered.Nack(false, true); err != nil {
								log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
							}

							continue mainWorkLoop
						}
					} else {
						if i < len(readBuffer) {
							readBuffer = readBuffer[:i]
						}
						if _, err = byteBuf.Write(readBuffer); err != nil {
							log.Errorf("Failed to write to read buffer for full read, reason: (%v)", err.Error())
							if err = delivered.Nack(false, true); err != nil {
								log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
							}

							continue mainWorkLoop
						}
					}

					// Write data to file
					if _, err = byteBuf.WriteTo(dest); err != nil {
						log.Errorf("Failed to write to archive file, reason: (%v)", err.Error())

						continue mainWorkLoop
					}
				}

				file.Close()
				dest.Close()

				fileInfo := database.FileInfo{}
				fileInfo.Path = fileID
				fileInfo.Checksum = hash
				fileInfo.Size, err = archive.GetFileSize(fileID)
				if err != nil {
					log.Errorf("Couldn't get file size from archive, reason: %v)", err.Error())
					if err = delivered.Nack(false, true); err != nil {
						log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
					}

					continue
				}

				log.Debugf("Wrote archived file (corr-id: %s, user: %s, filepath: %s, archivepath: %s, archivedsize: %d)",
					delivered.CorrelationId, message.User, message.FilePath, fileID, fileInfo.Size)

				status, err := db.GetFileStatus(delivered.CorrelationId)
				if err != nil {
					log.Errorf("failed to get file status, reason: %v", err.Error())
					if err = delivered.Nack(false, true); err != nil {
						log.Errorf("Failed to Nack message, reason: (%v)", err.Error())
					}
				}
				if status == "disabled" {
					log.Infof("file with correlation ID: %s is disabled, stopping ingestion", delivered.CorrelationId)
					if err := delivered.Ack(false); err != nil {
						log.Errorf("Failed acking canceled work, reason: %v", err.Error())
					}

					continue
				}

				if err := db.SetArchived(fileInfo, fileID, delivered.CorrelationId); err != nil {
					log.Errorf("SetArchived failed, reason: (%v)", err.Error())
				}

				log.Debugf("File marked as archived (corr-id: %s, user: %s, filepath: %s, archivepath: %s)",
					delivered.CorrelationId, message.User, message.FilePath, fileID)

				// Send message to archived
				msg := schema.IngestionVerification{
					User:        message.User,
					FilePath:    message.FilePath,
					FileID:      fileID,
					ArchivePath: fileID,
					EncryptedChecksums: []schema.Checksums{
						{Type: "sha256", Value: fmt.Sprintf("%x", hash.Sum(nil))},
					},
				}
				archivedMsg, _ := json.Marshal(&msg)

				err = schema.ValidateJSON(fmt.Sprintf("%s/ingestion-verification.json", conf.Broker.SchemasPath), archivedMsg)
				if err != nil {
					log.Errorf("Validation of outgoing message failed, reason: (%v)", err.Error())

					continue
				}

				if err := mq.SendMessage(delivered.CorrelationId, conf.Broker.Exchange, conf.Broker.RoutingKey, archivedMsg); err != nil {
					// TODO fix resend mechanism
					log.Errorf("failed to publish message, reason: (%v)", err.Error())

					// Do not try to ACK message to make sure we have another go
					continue
				}
				if err := delivered.Ack(false); err != nil {
					log.Errorf("failed to Ack message, reason: (%v)", err.Error())
				}
			}
		}
	}()

	<-forever
}

// tryDecrypt tries to decrypt the start of buf.
func tryDecrypt(key *[32]byte, buf []byte) ([]byte, error) {

	log.Debugln("Try decrypting the first data block")
	a := bytes.NewReader(buf)
	b, err := streaming.NewCrypt4GHReader(a, *key, nil)
	if err != nil {
		log.Error(err)

		return nil, err

	}
	_, err = b.ReadByte()
	if err != nil {
		log.Error(err)

		return nil, err
	}

	f := bytes.NewReader(buf)
	header, err := headers.ReadHeader(f)
	if err != nil {
		log.Error(err)

		return nil, err
	}

	return header, nil
}
