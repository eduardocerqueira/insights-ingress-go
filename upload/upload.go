package upload

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"sort"
	"time"

	"github.com/redhatinsights/insights-ingress-go/announcers"
	"github.com/redhatinsights/insights-ingress-go/config"
	"github.com/redhatinsights/insights-ingress-go/interactions/inventory"
	l "github.com/redhatinsights/insights-ingress-go/logger"
	"github.com/redhatinsights/insights-ingress-go/stage"
	"github.com/redhatinsights/insights-ingress-go/validators"
	"github.com/redhatinsights/platform-go-middlewares/identity"
	"github.com/redhatinsights/platform-go-middlewares/request_id"
	"github.com/sirupsen/logrus"
)

// GetFile verifies that the proper upload field is in place and returns the file
func GetFile(r *http.Request) (multipart.File, *multipart.FileHeader, error) {
	file, fileHeader, fileErr := r.FormFile("file")
	if fileErr == nil {
		return file, fileHeader, nil
	}
	file, fileHeader, uploadErr := r.FormFile("upload")
	if uploadErr == nil {
		return file, fileHeader, nil
	}
	keys := make([]string, 0, len(r.PostForm))
	for name := range r.PostForm {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return nil, nil, fmt.Errorf("Unable to find file (%v) or upload (%v) parts in %v", fileErr, uploadErr, keys)
}

func readMetadataPart(r *http.Request) ([]byte, error) {
	mdf, _, err := r.FormFile("metadata")
	if err == nil {
		defer mdf.Close()
		return ioutil.ReadAll(mdf)
	}
	metadata := r.FormValue("metadata")
	if metadata != "" {
		return []byte(metadata), nil
	}

	return nil, errors.New("Failed to find metadata as a file or value")
}

// GetMetadata returns metadata content from a file or value part
func GetMetadata(r *http.Request) (*validators.Metadata, error) {
	part, err := readMetadataPart(r)
	if err != nil {
		return nil, err
	}
	var md validators.Metadata
	if err = json.Unmarshal(part, &md); err != nil {
		return nil, err
	}
	return &md, nil
}

// NewHandler returns a http handler configured with a Pipeline
func NewHandler(
	stager stage.Stager,
	inventory inventory.Inventory,
	validator validators.Validator,
	tracker announcers.Announcer,
	cfg config.IngressConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userAgent := r.Header.Get("User-Agent")
		reqID := request_id.GetReqID(r.Context())
		requestLogger := l.Log.WithFields(logrus.Fields{"request_id": reqID})

		logerr := func(msg string, err error) {
			requestLogger.WithFields(logrus.Fields{"error": err}).Error(msg)
		}

		if cfg.Debug && cfg.DebugUserAgent.MatchString(userAgent) {
			dumpBytes, err := httputil.DumpRequest(r, true)
			if err != nil {
				logerr("debug: failed to dump request", err)
			} else {
				requestLogger.WithFields(logrus.Fields{"raw_request": dumpBytes}).Info("dumping request")
			}
		}

		incRequests(userAgent)
		file, fileHeader, err := GetFile(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("File or Upload field not found"))
			logerr("Invalid upload payload", err)
			return
		}
		observeSize(userAgent, fileHeader.Size)

		requestLogger.WithFields(logrus.Fields{"ContentType": fileHeader.Header.Get("Content-Type")}).Debug("ContentType received from client")
		serviceDescriptor, validationErr := getServiceDescriptor(fileHeader.Header.Get("Content-Type"))
		if validationErr != nil {
			logerr("Unable to validate", validationErr)
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}

		if fileHeader.Size > cfg.MaxSize {
			requestLogger.WithFields(logrus.Fields{"size": fileHeader.Size}).Info("File exceeds maximum file size for upload")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}

		if err := validator.ValidateService(serviceDescriptor); err != nil {
			logerr("Unrecognized service", err)
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}

		b64Identity := r.Header.Get("x-rh-identity")

		vr := &validators.Request{
			RequestID:   reqID,
			Size:        fileHeader.Size,
			Service:     serviceDescriptor.Service,
			Category:    serviceDescriptor.Category,
			B64Identity: b64Identity,
		}

		if config.Get().Auth == true {
			id := identity.Get(r.Context())
			vr.Account = id.Identity.AccountNumber
			vr.Principal = id.Identity.Internal.OrgID
		}

		md, err := GetMetadata(r)
		if err != nil {
			requestLogger.WithFields(logrus.Fields{"error": err}).Debug("Failed to read metadata")
		} else {
			vr.Metadata = *md
			vr.ID, err = inventory.GetID(*md, vr.Account, b64Identity)
			if err != nil {
				logerr("Failed to post to inventory", err)
			} else {
				requestLogger.WithFields(logrus.Fields{"inventory_id": vr.ID}).Info("Successfully posted to inventory")
			}
		}

		ps := &announcers.Status{
			Account:   vr.Account,
			RequestID: reqID,
			Status:    "received",
			StatusMsg: "Payload recived by ingress",
		}
		requestLogger.Info("Payload received")
		tracker.Status(ps)

		stageInput := &stage.Input{
			Payload: file,
			Key:     reqID,
			Account: vr.Account,
			OrgId:   vr.Principal,
		}

		start := time.Now()
		url, err := stager.Stage(stageInput)
		stageInput.Close()
		observeStageElapsed(time.Since(start))
		if err != nil {
			logerr("Error staging", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		vr.URL = url
		vr.Timestamp = time.Now()

		ps = &announcers.Status{
			Account:   vr.Account,
			RequestID: vr.RequestID,
			Status:    "success",
			StatusMsg: fmt.Sprintf("Sent to validation service: %s", vr.Service),
		}
		requestLogger.Info("Paylod sent to validation service")
		tracker.Status(ps)

		validator.Validate(vr)

		w.WriteHeader(http.StatusAccepted)
	}
}
