package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	instana "github.com/instana/go-sensor"
)

// Create global Instana sensor instance
var sensor = instana.NewSensor("details")

var incomingHeaders = []string{
	// All applications should propagate x-request-id. This header is
	// included in access log statements and is used for consistent trace
	// sampling and log sampling decisions in Istio.
	"x-request-id",

	// Lightstep tracing header. Propagate this if you use lightstep tracing
	// in Istio (see
	// https://istio.io/latest/docs/tasks/observability/distributed-tracing/lightstep/)
	// Note: this should probably be changed to use B3 or W3C TRACE_CONTEXT.
	// Lightstep recommends using B3 or TRACE_CONTEXT and most application
	// libraries from lightstep do not support x-ot-span-context.
	"x-ot-span-context",

	// Datadog tracing header. Propagate these headers if you use Datadog
	// tracing.
	"x-datadog-trace-id",
	"x-datadog-parent-id",
	"x-datadog-sampling-priority",

	// W3C Trace Context. Compatible with OpenCensusAgent and Stackdriver Istio
	// configurations.
	"traceparent",
	"tracestate",

	// Cloud trace context. Compatible with OpenCensusAgent and Stackdriver Istio
	// configurations.
	"x-cloud-trace-context",

	// Grpc binary trace context. Compatible with OpenCensusAgent nad
	// Stackdriver Istio configurations.
	"grpc-trace-bin",

	// b3 trace headers. Compatible with Zipkin, OpenCensusAgent, and
	// Stackdriver Istio configurations.
	"x-b3-traceid",
	"x-b3-spanid",
	"x-b3-parentspanid",
	"x-b3-sampled",
	"x-b3-flags",

	// Application-specific headers to forward.
	"end-user",
	"user-agent",
}

type Details struct {
	Id        int    `json:"id"`
	Author    string `json:"author"`
	Year      int    `json:"year"`
	Type      string `json:"type"`
	Pages     int    `json:"pages"`
	Publisher string `json:"publisher"`
	Language  string `json:"language"`
	ISBN_10   string `json:"isbn-10"`
	ISBN_13   string `json:"isbn-13"`
}

func health(w http.ResponseWriter, req *http.Request) {
	data, _ := json.Marshal(&struct {
		Status string `json:"status"`
	}{"Details is healthy"})
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(200)
	fmt.Fprint(w, string(data))
}

func details(w http.ResponseWriter, req *http.Request) {
	pathParts := strings.Split(req.URL.Path, "/")
	id, err := strconv.Atoi(pathParts[len(pathParts)-1])
	headers := getForwardHeaders(req)
	w.Header().Add("Content-Type", "application/json")
	var data []byte
	if err != nil {
		data, _ = json.Marshal(&struct {
			Error string `json:"error"`
		}{"please provide numeric product id"})
		w.WriteHeader(400)
	} else {
		data, _ = json.Marshal(getBookDetails(id, headers, req.Context()))
	}
	fmt.Fprint(w, string(data))
}

func getBookDetails(id int, headers http.Header, ctx context.Context) *Details {
	if os.Getenv("ENABLE_EXTERNAL_BOOK_SERVICE") == "true" {
		isbn := "0486424618"
		return fetchDetailsFromExternalService(isbn, id, headers, ctx)
	}
	return &Details{
		Id:        id,
		Author:    "William Shakespeare",
		Year:      1595,
		Type:      "paperback",
		Pages:     200,
		Publisher: "PublisherA",
		Language:  "English",
		ISBN_10:   "1234567890",
		ISBN_13:   "123-1234567890",
	}
}

func fetchDetailsFromExternalService(isbn string, id int, headers http.Header, ctx context.Context) *Details {
	proto := "https"
	if os.Getenv("DO_NOT_ENCRYPT") == "true" {
		proto = "http"
	}
	uri := proto + "://www.googleapis.com/books/v1/volumes?q=isbn:" + isbn
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return &Details{}
	}
	client := &http.Client{Transport: instana.RoundTripper(sensor, tr), Timeout: 5 * time.Second}
	res, err := client.Do(req.WithContext(ctx))
	if err != nil {
		fmt.Println(err)
		return &Details{}
	}
	if res.StatusCode != 200 {
		fmt.Println(res.Status)
		return &Details{}
	}
	defer res.Body.Close()

	rec := &struct {
		Items []struct {
			VolumeInfo struct {
				Language            string   `json:"language"`
				PrintType           string   `json:"printType"`
				Authors             []string `json:"authors"`
				Publisher           string   `json:"publisher"`
				PageCount           int      `json:"pageCount"`
				PublishedDate       string   `json:"publishedDate"`
				IndustryIdentifiers []struct {
					Type       string `json:"type"`
					Identifier string `json:"identifier"`
				} `json:"industryIdentifiers"`
			} `json:"volumeInfo"`
		} `json:"items"`
	}{}

	json.NewDecoder(res.Body).Decode(rec)
	book := rec.Items[0].VolumeInfo

	language, printType := "unknown", "unknown"
	if book.PrintType == "BOOK" {
		printType = "paperback"
	}
	if book.Language == "en" {
		language = "English"
	}
	isbnIdentifier := make(map[string]string)
	for _, item := range book.IndustryIdentifiers {
		isbnIdentifier[item.Type] = item.Identifier
	}
	year, _ := strconv.Atoi(book.PublishedDate)

	return &Details{
		Id:        id,
		Author:    book.Authors[0],
		Year:      year,
		Type:      printType,
		Pages:     book.PageCount,
		Publisher: book.Publisher,
		Language:  language,
		ISBN_10:   isbnIdentifier["ISBN_10"],
		ISBN_13:   isbnIdentifier["ISBN_13"],
	}
}

func getForwardHeaders(req *http.Request) http.Header {
	header := http.Header{}
	for _, item := range incomingHeaders {
		if values := req.Header.Values(item); len(values) > 0 {
			header[item] = values
		}
	}
	return header
}

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("usage: %v port\n", os.Args[0])
		os.Exit(-1)
	}

	port := os.Args[1]

	// Catch SIGTERM
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		<-sig
		os.Exit(0)
	}()

	http.HandleFunc("/details/", instana.TracingHandlerFunc(sensor, "/details", details))
	http.HandleFunc("/details", instana.TracingHandlerFunc(sensor, "/details", details))
	http.HandleFunc("/health", instana.TracingHandlerFunc(sensor, "/health", health))

	http.ListenAndServe(":"+port, nil)
}
