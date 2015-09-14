package main

import (
	"testing"
)

func TestEventSourceClientQueryProcess(t *testing.T) {

	t.Log("auieaiue")

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
		t.Fatal()
	}
}
