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
	"strings"
	"time"
)

var c <-chan time.Time

func main() {
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("/", eventSourceHandler)
	c = time.Tick(1 * time.Second)
	go http.ListenAndServe(":8888", publicMux)

	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/message", internalHandler)
	log.Fatal(http.ListenAndServe(":8889", internalMux))

}

func sendTicks() {
	for {
		log.Println("clients :", len(EventSourceClients))

		s := <-c

		for _, v := range EventSourceClients {
			v.Write(
				EventSourceMessage{
					message:   s.String(),
					eventType: "date",
				})
		}
	}
}

func displayProcessingTime(start time.Time, note string) {
	log.Println("processing time for", note, time.Since(start))
}

var start time.Time

func internalHandler(w http.ResponseWriter, r *http.Request) {
	defer displayProcessingTime(time.Now(), "internalHandler")
	start = time.Now()

	// check if the request is POST
	if r.Method != "POST" {
		log.Println("Only POST request accepted")
		http.Error(w, "Only POST request accepted", http.StatusForbidden)
		return
	}

	displayProcessingTime(start, "internalHandler 2")

	// we read the requst body
	body, err := ioutil.ReadAll(r.Body)

	if err != nil {
		log.Println(err)
		http.Error(w, "Unable to get request body", http.StatusBadRequest)
		return
	}
	displayProcessingTime(start, "internalHandler 3")

	mr := MessageRequest{}
	err = json.Unmarshal(body, &mr)
	if err != nil {
		log.Println(err)
		http.Error(w, "Unable to decode json", http.StatusBadRequest)
		return
	}
	displayProcessingTime(start, "internalHandler 4")

	client, present := EventSourceClients[mr.Id]
	if !present {
		http.Error(w, fmt.Sprintf("Client %s not found", mr.Id), http.StatusNotFound)
		log.Printf("Client %s not found", mr.Id)
		return
	}

	displayProcessingTime(start, "internalHandler 5")
	client.Write(EventSourceMessage{message: mr.Data})
	displayProcessingTime(start, "internalHandler 6")

	//	client.Close()
}

type MessageRequest struct {
	Id   string `json:"id"`
	Data string `json:"data"`
}

// TODO : protect mutual access
var EventSourceClients = map[string]EventSourceClient{}

type EventSourceMessage struct {
	message   string
	eventType string
}

type EventSourceClient struct {
	input chan EventSourceMessage
	close chan bool
}

func NewEventSourceClient() (esc EventSourceClient) {
	displayProcessingTime(start, "internalHandler 6_1")

	return EventSourceClient{
		input: make(chan EventSourceMessage),
		close: make(chan bool),
	}
}

func (esc EventSourceClient) Write(m EventSourceMessage) {
	displayProcessingTime(start, "internalHandler 6_2")
	esc.input <- m
}

func (esc EventSourceClient) Close() {
	esc.close <- true
}

func eventSourceHandler(w http.ResponseWriter, r *http.Request) {
	defer displayProcessingTime(time.Now(), "eventSourceHandler")

	//we get the client identifier
	id := strings.Split(r.URL.Path, "/")[1]
	if id == "" {
		http.Error(w, "no identifier provided", http.StatusNotFound)
		log.Println("no identifier provided")
		return
	}

	log.Println("connected :", id)

	// check if we don't overwrite an existing id
	_, present := EventSourceClients[id]
	if present {
		http.Error(w, "", http.StatusNotFound)
		log.Println("the client", id, "already exist. No overwriting")
		return
	}

	// we register as id
	chans := NewEventSourceClient()
	EventSourceClients[id] = chans
	// ensure that the client will be deleted at the end of his session
	defer delete(EventSourceClients, id)

	// gain access to the tcp connexion used
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

	// welcome message…
	_, err = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n"))
	if err != nil {
		return
	}

	ess := EventSourceSender{bufrw}

	for {
		select {
		case s := <-chans.input:
			displayProcessingTime(start, "internalHandler 7")
			if !aliveConn(conn, bufrw) {
				return
			}
			displayProcessingTime(start, "internalHandler 8")

			ess.Send(s)
			displayProcessingTime(start, "internalHandler 9")

		case <-chans.close:
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

// dirty but correct code for testing if the tcp connexion is alive as Russ Cox said : https://groups.google.com/d/msg/golang-nuts/oaKW4WMTdK8/3qiR2Mvn43kJ
func aliveConn(conn net.Conn, bufrw *bufio.ReadWriter) (open bool) {

	buf := make([]byte, 1)
	err := conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
	if err != nil {
		log.Println("unable to set deadline")
	}
	n, err := bufrw.Read(buf)

	if err != nil {
		if err == io.EOF {
			// connection closed
			log.Println("connection closed")
			return false
		} else {
			//			log.Println(err)
			return true
		}
	}

	log.Println("read :", n, buf[:n])

	return true
}
