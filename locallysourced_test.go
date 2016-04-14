package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"

	"testing"
)

func TestEventSourceClientQueryProcess(t *testing.T) {

	go EventSourceClientQueryProcess()

	callback0 := make(chan int)
	escCount <- EventSourceClientCountStruct{callback: callback0}
	count := <-callback0

	if count != 0 {
		t.Fatal("should be no client")
	}

	client := NewEventSourceClient("123")
	escAdd <- client

	escCount <- EventSourceClientCountStruct{callback: callback0}
	count = <-callback0

	if count != 1 {
		t.Fatal("missing client, expecting 1")
	}

	callback1 := make(chan []EventSourceClientAnswerStruct)
	escQuery <- EventSourceClientQueryStruct{callback: callback1, id: "123"}
	escList := <-callback1

	if len(escList) != 1 {
		t.Fatal("got no client, expecting 1")
	}

	if len(escList) == 1 && escList[0].present != true {
		t.Fatal("got a absent client")
	}

	if len(escList) == 1 && escList[0].client.id != "123" {
		t.Fatal("got another client id")
	}

	escQuery <- EventSourceClientQueryStruct{callback: callback1, id: ""}
	escList = <-callback1

	if len(escList) != 1 {
		t.Fatal("expecting 1 client, got ", len(escList))
	}

	client = NewEventSourceClient("®þæù€")
	escAdd <- client

	escQuery <- EventSourceClientQueryStruct{callback: callback1, id: ""}
	escList = <-callback1

	for _, esc := range escList {
		t.Log("Client id : ", esc.client.id)
	}

	if len(escList) != 2 {
		t.Fatal("expecting 2 client, got ", len(escList))
	}

	escDelete <- "123"

	escCount <- EventSourceClientCountStruct{callback: callback0}
	count = <-callback0

	if count != 1 {
		t.Fatal("removed one of two clients, expecting 1 got", count)
	}
}

func TestInternalHandler(t *testing.T) {

	server := httptest.NewServer(http.HandlerFunc(internalHandler))
	defer server.Close()

	res, err := http.Get(server.URL)

	if err == nil && res != nil && res.StatusCode == http.StatusOK {
		t.Fatal("Only POST request should be accepted")
	}

	res, err = http.Post(server.URL, "application/json", nil)
	if err != nil || (res != nil && res.StatusCode != http.StatusBadRequest) {
		t.Fatal("error : ", err.Error)
	}

	res, err = http.Post(server.URL, "application/json", bytes.NewBuffer([]byte(`{"Id":"123","Data":"broadcast"}`)))
	// at the moment no client is registred, so the server should return a NotFound
	if err != nil || (res != nil && res.StatusCode != http.StatusNotFound) {
		t.Fatal(err)
	}

	//	t.Log(res)
}

//func TestEventSourceHandler(t *testing.T) {
//
//	server := httptest.NewServer(http.HandlerFunc(eventSourceHandler))
//	defer server.Close()
//
//	res, err := http.Get(server.URL)
//	// no identifier is provided, so the server might return NotFound
//	if err != nil || (res != nil && res.StatusCode != http.StatusNotFound) {
//		t.Fatal(err)
//	}
//
//	go func() {
//
//		res, err = http.Get(server.URL + "/123")
//		// no identifier is provided, so the server might return NotFound
//		if err != nil || (res != nil && res.StatusCode != http.StatusNotFound) {
//			t.Fatal(err)
//		}
//		t.Log("client goroutine fin")
//
//	}()
//	go func() {
//
//		_, _ = http.Get(server.URL + "/124")
//
//		t.Log("client goroutine fin")
//
//	}()
//	t.Log("avant création serveur")
//
//	serverInternal := httptest.NewServer(http.HandlerFunc(internalHandler))
//	defer serverInternal.Close()
//
//	t.Log("avant")
//	client := &http.Client{}
//	t.Log("avant 2, url :", serverInternal.URL)
//	request, err := http.NewRequest("POST", serverInternal.URL, bytes.NewBuffer([]byte(`{"Id":"","Data":"broadcast"}`)))
//	t.Log("avant 3")
//
//	resInt, err := client.Do(request)
//	//	resInt, err := client.Post(serverInternal.URL, "application/json", bytes.NewBuffer([]byte(`{"Id":"","Data":"broadcast"}`)))
//	t.Log("après call")
//	// at the moment no client is registred, so the server should return a NotFound
//	if err != nil || (resInt != nil && resInt.StatusCode != http.StatusNotFound) {
//		t.Log("traitement erreur ", resInt)
//
//		t.Fatal(err)
//	}
//
//	t.Fatal("après traitement erreur")
//
//}
