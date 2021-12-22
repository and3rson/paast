package main

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/speps/go-hashids/v2"
)

const MaxBodyLen = 1<<20
const PasteCooldown = 5 * time.Second
const Alphabet = "abcdefghijklmnopqrstuvwxyz1234567890"
const DataDir = "/var/lib/paast"
const ManpageText =
`NAME
	paast - create pastes with different methods

SYNOPSIS
	cat code.txt | curl {HOST} --data-binary @-
	cat code.txt | curl {HOST} -F 'foo=<-'
	cat code.txt | curl {HOST} -F '=<-'
	cat code.txt | http {HOST}

LIMITS
	Maximum allowed request body size is 1 MB.
	Creating pastes has a 5-second cooldown.

STATUS CODES
	200 - paste created, URL returned in response
	400 - bad request or empty paste input
	413 - paste input too large
	429 - attempt to create too many pastes, please wait 5 seconds
	500 - internal server error

AUTHOR
	Created by Andrew Dunai.
	Send your ideas & feedback to ` + "`echo YUBkdW4uYWk= | base64 -d`" + `

WWW
	https://dun.ai
`

var idSalt = os.Getenv("ID_SALT")
var addrTimeMap = map[string]time.Time{}

func ReadCounter(file *os.File) (int64, error) {
	content, err := ioutil.ReadAll(file)
	if err != nil {
		return 0, fmt.Errorf("read counter: %s", err)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
	if err != nil {
		return 0, nil
	}
	return value, nil
}

func WriteCounter(file *os.File, value int64) error {
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("write counter: %s", err)
	}
	if _, err := file.Write([]byte(fmt.Sprint(value))); err != nil {
		return fmt.Errorf("write counter: %s", err)
	}
	return nil
}

type HttpRoutes struct {
	hashidMaker *hashids.HashID
	lock sync.Mutex
}

func NewHttpRoutes() *HttpRoutes {
	hr := &HttpRoutes{}
	hashidData := hashids.NewData()
	hashidData.Salt = idSalt
	hashidData.Alphabet = Alphabet
	hashidData.MinLength = 3
	hashidMaker, err := hashids.NewWithData(hashidData)
	if err != nil {
		log.Fatal(err)
	}
	hr.hashidMaker = hashidMaker
	return hr
}

func (*HttpRoutes) Manpage(rw http.ResponseWriter, r *http.Request) {
	rw.WriteHeader(200)
	rw.Write([]byte(strings.ReplaceAll(ManpageText, "{HOST}", r.Host)))
}

func PasteFromMultipart(r *http.Request) ([]byte, error) {
	var err error
	var mr *multipart.Reader
	if mr, err = r.MultipartReader(); err != nil {
		return nil, err
	}
	var part *multipart.Part
	part, err = mr.NextPart()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no parts in multipart body")
		}
		return nil, err
	}
	return ioutil.ReadAll(part)
}

func PasteFromBody(r *http.Request) ([]byte, error) {
	return ioutil.ReadAll(r.Body)
}

func (hr *HttpRoutes) CreatePaste(rw http.ResponseWriter, r *http.Request) {
	defer func() {
		if r := recover(); r != nil {
			var msg string
			if err, ok := r.(error); ok {
				msg = err.Error()
			} else {
				msg = fmt.Sprint(r)
			}
			rw.WriteHeader(500)
			rw.Write([]byte(msg))
		}
	}()

	hr.lock.Lock()
	defer hr.lock.Unlock()

	var err error

	// Limit maximum request body size
	r.Body = http.MaxBytesReader(rw, r.Body, MaxBodyLen)

	// Parse request
	var pasteContent []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		pasteContent, err = PasteFromMultipart(r)
	// } else if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
	} else {
		pasteContent, err = PasteFromBody(r)
	}
	if err != nil {
		// https://github.com/golang/go/issues/30715
		if strings.HasSuffix(err.Error(), "http: request body too large") {
			rw.WriteHeader(413)
			rw.Write([]byte("error: request body too large\n"))
			return
		}
		panic(err)
	}

	if len(pasteContent) == 0 {
		rw.WriteHeader(400)
		rw.Write([]byte("error: your paste is empty!\n"))
		return
	}

	// Read counter
	var counterFile *os.File
	var counter int64
	var counterHash string
	if counterFile, err = os.OpenFile(path.Join(DataDir, "counter.dat"), os.O_CREATE | os.O_RDWR, 0644); err != nil {
		panic(err)
	}
	defer counterFile.Close()
	if counter, err = ReadCounter(counterFile); err != nil {
		panic(err)
	}
	counter++
	if err = WriteCounter(counterFile, counter); err != nil {
		panic(err)
	}

	// Generate hash
	if counterHash, err = hr.hashidMaker.EncodeInt64([]int64{counter}); err != nil {
		panic(err)
	}

	// Save paste
	var pasteFile *os.File
	if pasteFile, err = os.OpenFile(
		path.Join(DataDir, fmt.Sprintf("pastes/%09d_%s", counter, counterHash)),
		os.O_CREATE | os.O_WRONLY, 0644,
	); err != nil {
		panic(err)
	}
	defer pasteFile.Close()
	if _, err = pasteFile.Write(pasteContent); err != nil {
		panic(err)
	}

	// Return URL
	scheme := "http"
	if r.URL.Scheme != "" {
		scheme = r.URL.Scheme
	}
	rw.WriteHeader(200)
	rw.Write([]byte(fmt.Sprintf("%s://%s/%s\n", scheme, r.Host, counterHash)))
}

func (hr *HttpRoutes) RetrievePaste(rw http.ResponseWriter, r *http.Request) {
	defer func() {
		if e, ok := recover().(error); ok {
			rw.WriteHeader(500)
			rw.Write([]byte(e.Error()))
		}
	}()

	var err error

	// Get hash from URL
	vars := mux.Vars(r)
	hash, _ := vars["hash"]

	// Read paste from file
	var pasteFile *os.File
	var content []byte
	counters, _ := hr.hashidMaker.DecodeInt64WithError(hash)
	if len(counters) == 0 {
		counters = append(counters, 0)
	}
	if pasteFile, err = os.OpenFile(
		path.Join(DataDir, fmt.Sprintf("pastes/%09d_%s", counters[0], hash)),
		os.O_RDONLY, 0644,
	); err != nil {
		if os.IsNotExist(err) {
			rw.WriteHeader(404)
			rw.Write([]byte(fmt.Sprintf("paste with id \"%s\" was not found\n", hash)))
			return
		}
		panic(err)
	}
	defer pasteFile.Close()
	if content, err = ioutil.ReadAll(pasteFile); err != nil {
		panic(err)
	}

	// Return content
	rw.WriteHeader(200)
	rw.Write(content)
}

func RateLimit(fn http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		addrParts := strings.Split(r.RemoteAddr, ":")
		if len(addrParts) > 1 {
			lastTime := addrTimeMap[addrParts[0]]
			nextTry := lastTime.Add(PasteCooldown)
			retryAfter := int64(math.Ceil(time.Until(nextTry).Seconds()))
			if retryAfter > 0 {
				rw.Header().Add("Retry-After", fmt.Sprint(retryAfter))
				rw.WriteHeader(429)
				rw.Write([]byte(fmt.Sprintf(
					"error: please wait %d seconds before creating new paste\n",
					retryAfter,
				)))
				return
			}
			addrTimeMap[addrParts[0]] = time.Now()
		}
		fn(rw, r)
	}
}

func main() {
	httpRoutes := NewHttpRoutes()

	router := mux.NewRouter()
	router.Use(handlers.ProxyHeaders) // Required for X-Forwarded-Proto
	router.HandleFunc("/", httpRoutes.Manpage).Methods("GET")
	router.HandleFunc("/", RateLimit(httpRoutes.CreatePaste)).Methods("POST")
	router.HandleFunc(fmt.Sprintf("/{hash:[%s]+}", Alphabet), httpRoutes.RetrievePaste).Methods("GET")

	server := &http.Server{
		Addr:    "0.0.0.0:8080",
		Handler: nil,
	}
	http.Handle("/", router)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("server loop: %s\n", err)
	}
}
