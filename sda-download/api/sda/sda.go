package sda

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/neicnordic/crypt4gh/streaming"
	"github.com/neicnordic/sda-download/api/middleware"
	"github.com/neicnordic/sda-download/internal/config"
	"github.com/neicnordic/sda-download/internal/database"
	"github.com/neicnordic/sda-download/internal/reencrypt"
	"github.com/neicnordic/sda-download/internal/storage"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

var Backend storage.Backend

func sanitizeString(str string) string {
	var pattern = regexp.MustCompile(`(https?://[^\s/$.?#].[^\s]+|[A-Za-z0-9-_:.]+)`)

	return pattern.ReplaceAllString(str, "[identifier]: $1")
}

func reencryptHeader(oldHeader []byte, reencKey string) ([]byte, error) {
	var opts []grpc.DialOption
	if config.Config.Reencrypt.CACert != "" {
		cacert := config.Config.Reencrypt.CACert
		clientKey := config.Config.Reencrypt.ClientKey
		clientCert := config.Config.Reencrypt.ClientCert
		rootCAs := x509.NewCertPool()
		cacertByte, err := os.ReadFile(cacert)
		if err != nil {
			log.Errorf("Failed to read CA certificate, reason: %s", err)

			return nil, err
		}
		ok := rootCAs.AppendCertsFromPEM(cacertByte)
		if !ok {
			log.Errorf("Failed to append CA certificate to rootCAs")

			return nil, errors.New("failed to append CA certificate to rootCAs")
		}
		// Load the client key pair
		certs, err := tls.LoadX509KeyPair(clientCert, clientKey)
		if err != nil {
			log.Errorf("Failed to load client key pair for reencrypt, reason: %s", err)
			log.Debugf("clientCert: %s, clientKey: %s", clientCert, clientKey)

			return nil, err
		}
		clientCreds := credentials.NewTLS(
			&tls.Config{
				Certificates: []tls.Certificate{certs},
				MinVersion:   tls.VersionTLS13,
				RootCAs:      rootCAs,
			},
		)
		// Use secure gRPC connection with mutual TLS authentication
		opts = append(opts, grpc.WithTransportCredentials(clientCreds))
	} else {
		// Use insecure gRPC connection
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	address := fmt.Sprintf("%s:%d", config.Config.Reencrypt.Host, config.Config.Reencrypt.Port)
	log.Debugf("Address of the reencrypt service: %s", address)

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		log.Errorf("Failed to connect to the reencrypt service, reason: %s", err)

		return nil, err
	}
	defer conn.Close()
	log.Debugf("Connection to the reencrypt service established, conn = %v", conn)

	timeoutDuration := time.Duration(config.Config.Reencrypt.Timeout) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeoutDuration)
	defer cancel()

	c := reencrypt.NewReencryptClient(conn)
	log.Debugf("Client created, c = %v", c)
	res, err := c.ReencryptHeader(ctx, &reencrypt.ReencryptRequest{Oldheader: oldHeader, Publickey: reencKey})
	if err != nil {
		log.Errorf("Failed response from the reencrypt service, reason: %s", err)

		return nil, err
	}
	log.Debugf("Response from the reencrypt service: %v", res)

	return res.Header, nil
}

// Datasets serves a list of permitted datasets
func Datasets(c *gin.Context) {
	log.Debugf("request permitted datasets")

	// Retrieve dataset list from request context
	// generated by the authentication middleware
	cache := middleware.GetCacheFromContext(c)

	// Return response
	c.JSON(http.StatusOK, cache.Datasets)
}

// find looks for a dataset name in a list of datasets
func find(datasetID string, datasets []string) bool {
	found := false
	for _, dataset := range datasets {
		if datasetID == dataset {
			found = true

			break
		}
	}

	return found
}

// getFiles returns files belonging to a dataset
var getFiles = func(datasetID string, ctx *gin.Context) ([]*database.FileInfo, int, error) {

	// Retrieve dataset list from request context
	// generated by the authentication middleware
	cache := middleware.GetCacheFromContext(ctx)

	log.Debugf("request to process files for dataset %s", sanitizeString(datasetID))

	if find(datasetID, cache.Datasets) {
		// Get file metadata
		files, err := database.GetFiles(datasetID)
		if err != nil {
			// something went wrong with querying or parsing rows
			log.Errorf("database query failed for dataset %s, reason %s", sanitizeString(datasetID), err)

			return nil, 500, errors.New("database error")
		}

		return files, 200, nil
	}

	return nil, 404, errors.New("dataset not found")
}

// Files serves a list of files belonging to a dataset
func Files(c *gin.Context) {

	// get dataset parameter
	dataset := c.Param("dataset")

	if !strings.HasSuffix(dataset, "/files") {
		c.String(http.StatusNotFound, "API path not found, maybe /files is missing")

		return
	}

	// remove / prefix and /files suffix
	dataset = strings.TrimPrefix(dataset, "/")
	dataset = strings.TrimSuffix(dataset, "/files")

	// Get optional dataset scheme
	// A scheme can be delivered separately in a query parameter
	// as schemes may sometimes be problematic when they travel
	// in the path. A client can conveniently split the scheme with "://"
	// which results in 1 item if there is no scheme (e.g. EGAD) or 2 items
	// if there was a scheme (e.g. DOI)
	scheme := c.Query("scheme")
	schemeLogs := strings.ReplaceAll(scheme, "\n", "")
	schemeLogs = strings.ReplaceAll(schemeLogs, "\r", "")

	datasetLogs := strings.ReplaceAll(dataset, "\n", "")
	datasetLogs = strings.ReplaceAll(datasetLogs, "\r", "")
	if scheme != "" {
		log.Debugf("adding scheme=%s to dataset=%s", schemeLogs, datasetLogs)
		dataset = fmt.Sprintf("%s://%s", scheme, dataset)
		log.Debugf("new dataset=%s", datasetLogs)
	}

	// Get dataset files
	files, code, err := getFiles(dataset, c)
	if err != nil {
		c.String(code, err.Error())

		return
	}

	// Return response
	c.JSON(http.StatusOK, files)
}

// Download serves file contents as bytes
func Download(c *gin.Context) {

	// Get file ID from path
	fileID := c.Param("fileid")

	// Check user has permissions for this file (as part of a dataset)
	dataset, err := database.CheckFilePermission(fileID)
	if err != nil {
		c.String(http.StatusNotFound, "file not found")

		return
	}

	// Get datasets from request context, parsed previously by token middleware
	cache := middleware.GetCacheFromContext(c)

	// Verify user has permission to datafile
	permission := false
	for d := range cache.Datasets {
		if cache.Datasets[d] == dataset {
			permission = true

			break
		}
	}
	if !permission {
		log.Debugf("user requested to view file, but does not have permissions for dataset %s", dataset)
		c.String(http.StatusUnauthorized, "unauthorised")

		return
	}

	// Get file header
	fileDetails, err := database.GetFile(fileID)
	if err != nil {
		c.String(http.StatusInternalServerError, "database error")

		return
	}

	// Get query params
	qStart := c.DefaultQuery("startCoordinate", "0")
	qEnd := c.DefaultQuery("endCoordinate", "0")

	// Parse and verify coordinates are valid
	start, err := strconv.ParseInt(qStart, 10, 0)

	if err != nil {
		log.Errorf("failed to convert start coordinate %d to integer, %s", start, err)
		c.String(http.StatusBadRequest, "startCoordinate must be an integer")

		return
	}
	end, err := strconv.ParseInt(qEnd, 10, 0)
	if err != nil {
		log.Errorf("failed to convert end coordinate %d to integer, %s", end, err)
		c.String(http.StatusBadRequest, "endCoordinate must be an integer")

		return
	}
	if end < start {
		log.Errorf("endCoordinate=%d must be greater than startCoordinate=%d", end, start)
		c.String(http.StatusBadRequest, "endCoordinate must be greater than startCoordinate")

		return
	}

	switch c.Param("type") {
	case "encrypted":
		// calculate coordinates
		start, end, err = calculateEncryptedCoords(start, end, c.GetHeader("Range"), fileDetails)
		if err != nil {
			log.Errorf("Byte range coordinates invalid! %v", err)

			return
		}
		if start > 0 {
			// reading from an offset in encrypted file is not yet supported
			log.Errorf("Start coordinate for encrypted files not implemented! %v", start)
			c.String(http.StatusBadRequest, "Start coordinate for encrypted files not implemented!")

			return
		}
	default:
		// set the content-length for unencrypted files
		if start == 0 && end == 0 {
			c.Header("Content-Length", fmt.Sprint(fileDetails.DecryptedSize))
		} else {
			// Calculate how much we should read (if given)
			togo := end - start
			c.Header("Content-Length", fmt.Sprint(togo))
		}
	}

	// Get archive file handle
	file, err := Backend.NewFileReader(fileDetails.ArchivePath)
	if err != nil {
		log.Errorf("could not find archive file %s, %s", fileDetails.ArchivePath, err)
		c.String(http.StatusInternalServerError, "archive error")

		return
	}

	c.Header("Content-Type", "application/octet-stream")
	if c.GetBool("S3") {
		lastModified, err := time.Parse(time.RFC3339, fileDetails.LastModified)
		if err != nil {
			log.Errorf("failed to parse last modified time: %v", err)
			c.AbortWithStatus(http.StatusInternalServerError)

			return
		}

		c.Header("Content-Disposition", fmt.Sprintf("filename: %v", fileID))
		c.Header("ETag", fileDetails.DecryptedChecksum)
		c.Header("Last-Modified", lastModified.Format(http.TimeFormat))

		// set the user and server public keys that is send from htsget
		log.Debugf("Got to setting the headers: %s", c.GetHeader("client-public-key"))
		c.Header("Client-Public-Key", c.GetHeader("Client-Public-Key"))
		c.Header("Server-Public-Key", c.GetHeader("Server-Public-Key"))
	}

	if c.Request.Method == http.MethodHead {

		if c.Param("type") == "encrypted" {
			c.Header("Content-Length", fmt.Sprint(fileDetails.ArchiveSize))

			// set the length of the crypt4gh header for htsget
			c.Header("Server-Additional-Bytes", fmt.Sprint(bytes.NewReader(fileDetails.Header).Size()))
			// TODO figure out if client crypt4gh header will have other size
			// c.Header("Client-Additional-Bytes", ...)
		}

		return
	}

	// Prepare the file for streaming, encrypted or decrypted
	var encryptedFileReader io.Reader
	var fileStream io.Reader
	hr := bytes.NewReader(fileDetails.Header)
	encryptedFileReader = io.MultiReader(hr, file)

	switch c.Param("type") {
	case "encrypted":
		// The key provided in the header should be base64 encoded
		reencKey := c.GetHeader("Client-Public-Key")
		if strings.HasPrefix(c.GetHeader("User-Agent"), "htsget") {
			reencKey = c.GetHeader("Server-Public-Key")
		}
		if reencKey == "" {
			fileStream = encryptedFileReader
		} else {
			log.Debugf("Public key from the request header = %v", reencKey)
			log.Debugf("old c4gh file header = %v\n", fileDetails.Header)
			newHeader, err := reencryptHeader(fileDetails.Header, reencKey)
			if err != nil {
				log.Errorf("Failed to reencrypt the file header, reason: %v", err)
			}
			log.Debugf("Reencrypted c4gh file header = %v", newHeader)
			newHr := bytes.NewReader(newHeader)
			fileStream = io.MultiReader(newHr, file)
		}

	default:
		c4ghfileStream, err := streaming.NewCrypt4GHReader(encryptedFileReader, *config.Config.App.Crypt4GHKey, nil)
		defer c4ghfileStream.Close()
		if err != nil {
			log.Errorf("could not prepare file for streaming, %s", err)
			c.String(http.StatusInternalServerError, "file stream error")

			return
		}
		if start != 0 {
			// We don't want to read from start, skip ahead to where we should be
			_, err = c4ghfileStream.Seek(start, 0)
			if err != nil {
				log.Errorf("error occurred while finding sending start: %v", err)
				c.String(http.StatusInternalServerError, "an error occurred")

				return
			}
		}
		fileStream = c4ghfileStream
	}

	err = sendStream(fileStream, c.Writer, start, end)
	if err != nil {
		log.Errorf("error occurred while sending stream: %v", err)
		c.String(http.StatusInternalServerError, "an error occurred")

		return
	}
}

// used from: https://github.com/neicnordic/crypt4gh/blob/master/examples/reader/main.go#L48C1-L113C1
var sendStream = func(reader io.Reader, writer http.ResponseWriter, start, end int64) error {

	// Calculate how much we should read (if given)
	togo := end - start

	buf := make([]byte, 4096)

	// Loop until we've read what we should (if no/faulty end given, that's EOF)
	for end == 0 || togo > 0 {
		rbuf := buf

		if end != 0 && togo < 4096 {
			// If we don't want to read as much as 4096 bytes
			rbuf = buf[:togo]
		}
		r, err := reader.Read(rbuf)
		togo -= int64(r)

		// Nothing more to read?
		if err == io.EOF && r == 0 {
			// Fall out without error if we had EOF (if we got any data, do one
			// more lap in the loop)
			return nil
		}

		if err != nil && err != io.EOF {
			// An error we want to signal?
			return err
		}

		wbuf := rbuf[:r]
		for len(wbuf) > 0 {
			// Loop until we've written all that we could read,
			// fall out on error
			w, err := writer.Write(wbuf)

			if err != nil {
				return err
			}
			wbuf = wbuf[w:]
		}
	}

	return nil
}

// Calculates the start and end coordinats to use. If a range is set in HTTP headers,
// it will be used as is. If not, the functions parameters will be used,
// and adjusted to match the data block boundaries of the encrypted file.
var calculateEncryptedCoords = func(start, end int64, htsget_range string, fileDetails *database.FileDownload) (int64, int64, error) {
	if htsget_range != "" {
		startEnd := strings.Split(strings.TrimPrefix(htsget_range, "bytes="), "-")
		if len(startEnd) > 1 {
			a, err := strconv.ParseInt(startEnd[0], 10, 64)
			if err != nil {
				return 0, 0, err
			}
			b, err := strconv.ParseInt(startEnd[1], 10, 64)
			if err != nil {
				return 0, 0, err
			}
			if a > b {
				return 0, 0, fmt.Errorf("endCoordinate must be greater than startCoordinate")
			}

			return a, b, nil
		}
	}
	// Adapt end coordinate to follow the crypt4gh block boundaries
	headlength := bytes.NewReader(fileDetails.Header)
	bodyEnd := int64(fileDetails.ArchiveSize)
	if end > 0 {
		var packageSize float64 = 65564 // 64KiB+28, 28 is for chacha20_ietf_poly1305
		togo := end - start
		bodysize := math.Max(float64(togo-headlength.Size()), 0)
		endCoord := packageSize * math.Ceil(bodysize/packageSize)
		bodyEnd = int64(math.Min(float64(bodyEnd), endCoord))
	}

	return start, headlength.Size() + bodyEnd, nil
}
