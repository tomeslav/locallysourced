package main

import (
	"bufio"
	"bytes"
	"encoding/json"
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
)

func main() {
	go handleSignals()
	go EventSourceClientQueryProcess()

	publicMux := http.NewServeMux()
	publicMux.HandleFunc("/", eventSourceHandler)
	go http.ListenAndServe(":8888", publicMux)

	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/message", internalHandler)
	log.Fatal(http.ListenAndServe(":8889", internalMux))

}

func internalHandler(w http.ResponseWriter, r *http.Request) {
	// check if the request is POST
	if r.Method != "POST" {
		log.Println("Only POST request accepted")
		http.Error(w, "Only POST request accepted", http.StatusForbidden)
		return
	}

	// we read the requst body
	body, err := ioutil.ReadAll(r.Body)

	if err != nil {
		log.Println(err)
		http.Error(w, "Unable to get request body", http.StatusBadRequest)
		return
	}

	mr := MessageRequest{}
	err = json.Unmarshal(body, &mr)
	if err != nil {
		log.Println(err)
		http.Error(w, "Unable to decode json", http.StatusBadRequest)
		return
	}

	// is the id already registred ?
	callback := make(chan EventSourceClientAnswerStruct)
	escQuery <- EventSourceClientQueryStruct{callback: callback, id: mr.Id}
	esc := <-callback

	if !esc.present {
		http.Error(w, fmt.Sprintf("Client %s not found", mr.Id), http.StatusNotFound)
		log.Printf("Client %s not found", mr.Id)
		return
	}

	esc.client.Write(EventSourceMessage{message: mr.Data})

	esc.client.Close()
}

type MessageRequest struct {
	Id   string `json:"id"`
	Data string `json:"data"`
}

var escQuery = make(chan EventSourceClientQueryStruct)
var escAdd = make(chan EventSourceClient)
var escDelete = make(chan string)

type EventSourceClientQueryStruct struct {
	callback chan EventSourceClientAnswerStruct
	id       string
}

type EventSourceClientAnswerStruct struct {
	present bool
	client  EventSourceClient
}

// EventSourceClient server, over go's chans
// using chans and select in place of a mutex over EventSourceClient (the go way !)
func EventSourceClientQueryProcess() {

	// declaring a nice map for storing
	var EventSourceClients = map[string]EventSourceClient{}

	for {
		select {
		case query := <-escQuery:
			client, present := EventSourceClients[query.id]
			query.callback <- EventSourceClientAnswerStruct{client: client, present: present}
		case id := <-escDelete:
			delete(EventSourceClients, id)
		case client := <-escAdd:
			EventSourceClients[client.id] = client
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
		log.Println("no identifier provided")
		return
	}

	log.Println("connected :", id)

	// check if we don't overwrite an existing id
	callback := make(chan EventSourceClientAnswerStruct)
	escQuery <- EventSourceClientQueryStruct{callback: callback, id: id}
	esc := <-callback

	if esc.present {
		http.Error(w, "", http.StatusNotFound)
		log.Println("the client", id, "already exist. No overwriting")
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
			go func() { terminateCleanUp <- true }()
			return
		case <-terminateES: // terminated by cleanUp goroutine. just need to return
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
		log.Println("unable to set deadline", err)
	}
	n, err := bufrw.Read(buf)

	if err != nil {
		if err == io.EOF {
			return false
		} else {
			return true
		}
	}

	log.Println("read :", n, buf[:n])

	return true
}

func handleSignals() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGUSR1, syscall.SIGUSR2)

	for {
		s := <-c
		log.Println("Received signal:", s)

		var buffer bytes.Buffer

		fmt.Fprintf(&buffer, "Go version \t: %s\n", runtime.Version())
		fmt.Fprintf(&buffer, "GoRoutines \t: %d\n", runtime.NumGoroutine())
		memStats := &runtime.MemStats{}
		runtime.ReadMemStats(memStats)
		fmt.Fprintf(&buffer, "MemStats :\n")
		fmt.Fprintf(&buffer, "  Alloc \t: %d kbytes\n", (memStats.Alloc / 1000))
		fmt.Fprintf(&buffer, "  Sys \t\t: %d kbytes\n", (memStats.Sys / 1000))
		fmt.Fprintf(&buffer, "  HeapAlloc \t: %d kbytes\n", (memStats.HeapAlloc / 1000))
		fmt.Fprintf(&buffer, "  HeapInuse \t: %d kbytes\n", (memStats.HeapInuse / 1000))
		fmt.Fprintf(&buffer, "  StackInuse \t: %d kbytes\n", (memStats.StackInuse / 1000))
		fmt.Fprintf(&buffer, "  MSpanInuse \t: %d kbytes\n", (memStats.MSpanInuse / 1000))
		fmt.Fprintf(&buffer, "  MCacheInuse \t: %d kbytes\n", (memStats.MCacheInuse / 1000))
		fmt.Fprintf(&buffer, "  LastGC \t: %f s ago\n", time.Since(time.Unix(0, int64(memStats.LastGC))).Seconds())

		log.Printf("Stats : \n%s", buffer.String())
	}
}
