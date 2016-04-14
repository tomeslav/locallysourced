package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

var (
	confFile = flag.String("config", "/etc/event_server.json", "config file path")
	config   = &configStruct{
		ListenPublic:   ":8888",
		ListenInternal: ":8889",
		Log:            "stdout",
	}

	// a default logger is configured for logging early startup events
	l = log.New(os.Stderr, "", log.LstdFlags|log.Lshortfile)
)

func main() {
	// config parsing
	flag.Parse()
	err := parseConfigFile()
	if err != nil {
		l.Fatal(err)
	}

	// log configuration
	w, err := getLogWriter()
	if err != nil {
		l.Fatal(err)
	}
	defer (*w).Close()

	l = log.New(w, "", log.LstdFlags|log.Lshortfile)

	go handleSignals()
	go EventSourceClientQueryProcess()

	publicMux := http.NewServeMux()
	publicMux.HandleFunc("/", eventSourceHandler)
	go http.ListenAndServe(config.ListenPublic, publicMux)

	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/message", internalHandler)
	l.Fatal(http.ListenAndServe(config.ListenInternal, internalMux))

}

func internalHandler(w http.ResponseWriter, r *http.Request) {
	// check if the request is POST
	if r.Method != "POST" {
		l.Println("Only POST request accepted")
		http.Error(w, "Only POST request accepted", http.StatusForbidden)
		return
	}

	// we read the requst body
	body, err := ioutil.ReadAll(r.Body)

	if err != nil {
		l.Println(err)
		http.Error(w, "Unable to get request body", http.StatusBadRequest)
		return
	}

	mr := MessageRequest{}
	err = json.Unmarshal(body, &mr)
	if err != nil {
		l.Println(err)
		http.Error(w, "Unable to decode json", http.StatusBadRequest)
		return
	}

	// is the id already registred ?
	callback := make(chan []EventSourceClientAnswerStruct)
	escQuery <- EventSourceClientQueryStruct{callback: callback, id: mr.Id}
	escList := <-callback

	for _, esc := range escList {
		if !esc.present {
			http.Error(w, fmt.Sprintf("Client %s not found", esc.client.id), http.StatusNotFound)
			l.Printf("Client %s not found", esc.client.id)
			return
		}

		esc.client.Write(EventSourceMessage{message: mr.Data})

		l.Println("sent", utf8.RuneCountInString(mr.Data), "characters to id", esc.client.id)
	}
	l.Println("Finished sending")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "Finished sending\n\n\n\r")

}

type MessageRequest struct {
	Id   string `json:"id"`
	Data string `json:"data"`
}

var escQuery = make(chan EventSourceClientQueryStruct)
var escAdd = make(chan EventSourceClient)
var escDelete = make(chan string)
var escCount = make(chan EventSourceClientCountStruct)

type EventSourceClientQueryStruct struct {
	callback chan []EventSourceClientAnswerStruct
	id       string
}

type EventSourceClientAnswerStruct struct {
	present bool
	client  EventSourceClient
}

type EventSourceClientCountStruct struct {
	callback chan int
}

// EventSourceClient server, over go's chans
// using chans and select in place of a mutex over EventSourceClient (the go way !)
func EventSourceClientQueryProcess() {

	// declaring a nice map for storing
	var EventSourceClients = map[string]EventSourceClient{}

	for {
		// lovely select for ensuring atomic operations on EventSourceClients
		select {
		case query := <-escQuery:
			if query.id != "" {
				client, present := EventSourceClients[query.id]
				query.callback <- []EventSourceClientAnswerStruct{EventSourceClientAnswerStruct{client: client, present: present}}
			} else {
				clientsList := []EventSourceClientAnswerStruct{}
				for _, client := range EventSourceClients {
					clientsList = append(clientsList, EventSourceClientAnswerStruct{client: client, present: true})
				}
				query.callback <- clientsList

			}
		case id := <-escDelete:
			delete(EventSourceClients, id)
		case client := <-escAdd:
			EventSourceClients[client.id] = client
		case query := <-escCount:
			query.callback <- len(EventSourceClients)
		}
	}
}

type EventSourceMessage struct {
	message   string
	eventType string
}

type EventSourceClient struct {
	id    string
	input chan EventSourceMessage
	close chan bool
}

func NewEventSourceClient(id string) (esc EventSourceClient) {
	return EventSourceClient{
		id:    id,
		input: make(chan EventSourceMessage),
		close: make(chan bool),
	}
}

func (esc EventSourceClient) Write(m EventSourceMessage) {
	esc.input <- m
}

func (esc EventSourceClient) Close() {
	esc.close <- true
}

func eventSourceHandler(w http.ResponseWriter, r *http.Request) {

	//we get the client identifier
	id := strings.Split(r.URL.Path, "/")[1]
	if id == "" {
		http.Error(w, "no identifier provided", http.StatusNotFound)
		l.Println("no identifier provided")
		return
	}

	l.Println("connected :", id)
	defer func() { l.Println("disconnected :", id) }()

	// check if we don't overwrite an existing id
	callback := make(chan []EventSourceClientAnswerStruct)
	escQuery <- EventSourceClientQueryStruct{callback: callback, id: id}
	esc := <-callback

	if len(esc) == 1 && esc[0].present {
		http.Error(w, "", http.StatusNotFound)
		l.Println("the client", id, "already exist. No overwriting")
		return
	}

	// we register as id
	client := NewEventSourceClient(id)
	escAdd <- client
	// ensure that the client will be deleted at the end of his session
	defer func() { escDelete <- id }()

	// gain access to the tcp connection used
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "webserver doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Don't forget to close the connection:
	defer conn.Close()

	// we setup a cleanup goroutine, checking every 10 seconds
	terminateES := make(chan bool)      // for terminating the current goroutine
	terminateCleanUp := make(chan bool) // for terminating cleanUp
	go cleanUp(conn, bufrw, terminateES, terminateCleanUp)

	// welcome message…
	_, err = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n"))
	if err != nil {
		return
	}

	// we use an eventsource wrapper
	ess := EventSourceSender{bufrw}

	for {
		select {
		case s := <-client.input:
			ess.Send(s)
		case <-client.close: // terminated by the outer api, need to cleanup the cleanup process
			l.Println("terminated by the outer api, need to cleanup the cleanup process")
			go func() { terminateCleanUp <- true }()
			return
		case <-terminateES: // terminated by cleanUp goroutine. just need to return
			l.Println("terminated by cleanUp goroutine. just need to return")
			return
		}
	}
	return
}

type EventSourceSender struct {
	w WriteFlusher
}

type WriteFlusher interface {
	io.Writer
	Flush() error
}

func (e EventSourceSender) Send(esm EventSourceMessage) {
	buf := new(bytes.Buffer)
	if esm.eventType != "" {
		fmt.Fprintf(buf, "event: %s\n", esm.eventType)
	}

	fmt.Fprintf(buf, "data: %s\n", esm.message)
	io.Copy(e.w, buf)
	e.w.Flush()
}

// check if the connection is still alive every 10 seconds, unless a message is received on terminateCleanUp, meaning that the connection is already closed by the parent goroutine
func cleanUp(conn net.Conn, bufrw *bufio.ReadWriter, terminateES chan bool, terminateCleanUp chan bool) {
	for {
		select {
		case <-terminateCleanUp:
			return
		default:
			if !aliveConn(conn, bufrw) {
				terminateES <- true
				return
			}
			time.Sleep(10 * time.Second)
		}
	}
}

// dirty but correct code for testing if the tcp connection is alive as Russ Cox said : https://groups.google.com/d/msg/golang-nuts/oaKW4WMTdK8/3qiR2Mvn43kJ
func aliveConn(conn net.Conn, bufrw *bufio.ReadWriter) (open bool) {

	buf := make([]byte, 1)
	err := conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if err != nil {
		l.Println("unable to set deadline", err)
	}
	n, err := bufrw.Read(buf)

	if err != nil {
		if err == io.EOF {
			return false
		} else {
			return true
		}
	}

	l.Println("read :", n, buf[:n])

	return true
}

func handleSignals() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR1, syscall.SIGUSR2)

	for {
		s := <-c
		l.Println("Received signal:", s)

		var buffer bytes.Buffer

		callback := make(chan int)
		escCount <- EventSourceClientCountStruct{callback: callback}
		count := <-callback

		fmt.Fprintf(&buffer, "Clients count\t: %d\n", count)
		fmt.Fprintf(&buffer, "Go version\t: %s\n", runtime.Version())
		fmt.Fprintf(&buffer, "GoRoutines\t: %d\n", runtime.NumGoroutine())
		memStats := &runtime.MemStats{}
		runtime.ReadMemStats(memStats)
		fmt.Fprintf(&buffer, "MemStats:\n")
		fmt.Fprintf(&buffer, "  Alloc\t\t: %d kbytes\n", (memStats.Alloc / 1000))
		fmt.Fprintf(&buffer, "  Sys\t\t: %d kbytes\n", (memStats.Sys / 1000))
		fmt.Fprintf(&buffer, "  HeapAlloc\t: %d kbytes\n", (memStats.HeapAlloc / 1000))
		fmt.Fprintf(&buffer, "  HeapInuse\t: %d kbytes\n", (memStats.HeapInuse / 1000))
		fmt.Fprintf(&buffer, "  StackInuse\t: %d kbytes\n", (memStats.StackInuse / 1000))
		fmt.Fprintf(&buffer, "  MSpanInuse\t: %d kbytes\n", (memStats.MSpanInuse / 1000))
		fmt.Fprintf(&buffer, "  MCacheInuse\t: %d kbytes\n", (memStats.MCacheInuse / 1000))
		fmt.Fprintf(&buffer, "  LastGC\t: %f s ago\n", time.Since(time.Unix(0, int64(memStats.LastGC))).Seconds())
		prettyConf, err := config.String()
		if err != nil {
			fmt.Fprintf(&buffer, "Config \t\t: %+v\n", *config)
		} else {
			fmt.Fprintf(&buffer, "Config :\n%s", prettyConf)
		}

		l.Printf("Stats : \n%s", buffer.String())
	}
}

// config handleing
type configStruct struct {
	ListenPublic   string // listen on address
	ListenInternal string // listen on address
	Log            string // logfile. use stdout or stderr for standard output/error
}

func (c *configStruct) String() (str string, err error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	return fmt.Sprintf("%s", b), nil
}

// parse the config file specified in confFile global var, and fill config
func parseConfigFile() (err error) {

	f, err := os.Open(*confFile)
	if err != nil {
		err = errors.New(fmt.Sprintf("Error opening config : %s", err.Error()))
		return
	}
	defer f.Close()

	dec := json.NewDecoder(f)

	err = dec.Decode(&config)
	if err != nil {
		err = errors.New(fmt.Sprintf("Error decoding config : %s", err.Error()))
		return
	}
	return
}

func getLogWriter() (w *os.File, err error) {
	switch config.Log {
	case "stdout":
		w = os.Stdout
	case "stderr":
		w = os.Stderr
	default:
		w, err = os.OpenFile(config.Log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)

		if err != nil {
			err = errors.New(fmt.Sprintf("Error opening config : %s", err.Error()))
			return
		}
	}
	return
}
