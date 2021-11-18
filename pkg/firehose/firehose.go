package firehose

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/heptiolabs/healthcheck"

	"github.com/IBM/fluent-forward-go/fluent/protocol"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	muxlogrus "github.com/pytimer/mux-logrus"

	fluentclient "github.com/IBM/fluent-forward-go/fluent/client"
	log "github.com/sirupsen/logrus"
)

const (
	accessKeyHeaderName        = "X-Amz-Firehose-Access-Key"
	requestIDHeaderName        = "X-Amz-Firehose-Request-Id"
	eventTypeHeaderName        = "X-Event-Type"
	commonAttributesHeaderName = "X-Amz-Firehose-Common-Attributes"
)

type APIError interface {
	APIError() (int, string, string)
}

type firehoseAPIError struct {
	code      int
	msg       string
	requestID string
}

func (e firehoseAPIError) Error() string {
	return e.msg
}

func (e firehoseAPIError) APIError() (int, string, string) {
	return e.code, e.msg, e.requestID
}

var (
	errAuth       = &firehoseAPIError{code: http.StatusUnauthorized, msg: "unauthorized"}
	errBadReq     = &firehoseAPIError{code: http.StatusBadRequest, msg: "bad request"}
	forwardClient *fluentclient.Client
	accessKey     string
)

// firehoseCommonAttributes represents common attributes (metadata).
type firehoseCommonAttributes struct {
	CommonAttributes map[string]string `json:"commonAttributes"`
}

// firehoseRequestBody represents request body.
type firehoseRequestBody struct {
	RequestID string           `json:"requestId,omitempty"`
	Timestamp int64            `json:"timestamp,omitempty"`
	Records   []firehoseRecord `json:"records,omitempty"`
}

// firehoseRecord represents records in request body.
type firehoseRecord struct {
	Data []byte `json:"data"`
}

// firehoseResponseBody represents response body.
type firehoseResponseBody struct {
	RequestID    string `json:"requestId,omitempty"`
	Timestamp    int64  `json:"timestamp,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

func RunFirehoseServer(address, key, forwardAddress string) {
	accessKey = key
	forwardHost, forwardPort, err := net.SplitHostPort(forwardAddress)
	if err != nil {
		log.Fatalf("Failed to parse forward address: %s", err)
	}
	forwardPortInt, _ := strconv.Atoi(forwardPort)
	forwardClient = &fluentclient.Client{
		ConnectionFactory: &fluentclient.TCPConnectionFactory{
			Target: fluentclient.ServerAddress{
				Hostname: forwardHost,
				Port:     forwardPortInt,
			},
		},
	}
	err = forwardClient.Connect()
	if err != nil {
		log.Fatalf("error connecting to fluent forwarder: %s", err)
	}

	log.Infof("Fluenthose server listening on %s", address)
	log.Debugf("log-level: %s, fowarding to: %s", log.GetLevel(), forwardAddress)

	health := healthcheck.NewHandler()

	health.AddLivenessCheck(
		"forwarder",
		healthcheck.TCPDialCheck(forwardAddress, 50*time.Millisecond))
	health.AddReadinessCheck(
		"forwarder",
		healthcheck.TCPDialCheck(forwardAddress, 50*time.Millisecond))

	logOptions := muxlogrus.LogOptions{
		Formatter:      &log.JSONFormatter{},
		EnableStarting: true,
	}
	loggingMiddleware := muxlogrus.NewLogger(logOptions)

	router := mux.NewRouter()
	router.Handle("/", loggingMiddleware.Middleware(http.HandlerFunc(firehoseHandler)))
	router.Handle("/metrics", promhttp.Handler())
	router.HandleFunc("/health/live", health.LiveEndpoint)
	router.HandleFunc("/health/ready", health.ReadyEndpoint)

	srv := &http.Server{
		Addr:    address,
		Handler: router,
	}
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	<-done

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		forwardClient.Disconnect()
		cancel()
	}()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server Shutdown Failed:%+v", err)
	}
	log.Infof("fluenthose Exited Properly")
}

func firehoseHandler(w http.ResponseWriter, r *http.Request) {
	log.Infof("firehose %s request received from %s", r.Method, r.RemoteAddr)
	log.Debugf("firehose request headers: %+v", r.Header)

	if r.Method != http.MethodPost {
		JSONHandleError(w, errBadReq)
		return
	}
	key := r.Header.Get(accessKeyHeaderName)
	if key == "" || key != accessKey {
		JSONHandleError(w, errAuth)
		return

	}
	requestID := r.Header.Get(requestIDHeaderName)
	if requestID == "" {
		JSONHandleError(w, errBadReq)
		return
	}
	resp := firehoseResponseBody{
		RequestID: requestID,
	}

	var eventType = "unknown"
	commonAttributes := firehoseCommonAttributes{}
	if err := json.Unmarshal([]byte(r.Header.Get(commonAttributesHeaderName)), &commonAttributes); err != nil {
		log.Errorf("failed to parse common attributes: %s", err)
	}

	if commonAttributes.CommonAttributes != nil {
		for k, v := range commonAttributes.CommonAttributes {
			log.Debugf("common attribute: %s=%s", k, v)
			if k == eventTypeHeaderName {
				eventType = v
				log.Debugf("set event type to: %s", v)
				break
			}
		}
		log.Debugf("event type is: %s", eventType)
	}

	firehoseReq, err := parseRequestBody(r)
	if err != nil {
		log.Errorf("failed to parse request body: %s", err)
		JSONHandleError(w, errBadReq)
		return
	}
	var recordCount = 0
	for recordCount, record := range firehoseReq.Records {
		log.Debugf("firehose record: %s", string(record.Data))
		msg := &protocol.Message{
			Tag:       eventType,
			Timestamp: time.Now().UTC().Unix(),
			Record: map[string]interface{}{
				"data": string(record.Data),
				"type": eventType,
			},
			Options: &protocol.MessageOptions{},
		}
		err := forwardClient.SendMessage(msg)
		if err != nil {
			log.Errorf("failed to send message: %s", err)
		}
		recordCount++
	}
	log.Infof("%d records sent to fluent forwarder", recordCount)

	resp.Timestamp = time.Now().UnixNano() / int64(time.Millisecond)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

func parseRequestBody(r *http.Request) (*firehoseRequestBody, error) {
	body := firehoseRequestBody{}
	logBody, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorf("failed to read request body: %s", err)
	}
	log.Debugf("request body: %s", string(logBody))
	r.Body = ioutil.NopCloser(bytes.NewBuffer(logBody))
	if r.Body == nil {
		log.Errorf("request body is empty")
		return nil, errBadReq
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Errorf("failed to decode request body: %s", err)
		return nil, errBadReq
	}
	return &body, nil
}

func JSONHandleError(w http.ResponseWriter, err error) {
	log.Debugf("Firehose error response: %s", err)
	jsonError := func(err APIError) *firehoseResponseBody {
		_, msg, requestID := err.APIError()
		return &firehoseResponseBody{
			ErrorMessage: msg,
			Timestamp:    time.Now().UnixNano() / int64(time.Millisecond),
			RequestID:    requestID,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err, ok := err.(APIError); ok {
		code, _, _ := err.APIError()
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(jsonError(err))
	} else {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(jsonError(&firehoseAPIError{msg: "internal server error"}))
	}
}
