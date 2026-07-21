package runner

import (
	"io"
	"log/slog"
	"testing"
)

func TestUnregisterClosesQAChannelsForDisconnectedRunner(t *testing.T) {
	hub := NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	disconnected := make(chan *QAEvent)
	other := make(chan *QAEvent)
	hub.qaCh["qa_disconnected"] = disconnected
	hub.qaConn["qa_disconnected"] = 7
	hub.qaCh["qa_other"] = other
	hub.qaConn["qa_other"] = 8

	hub.Unregister(7)

	if _, ok := <-disconnected; ok {
		t.Fatal("disconnected runner QA channel is still open")
	}
	if _, ok := hub.qaCh["qa_disconnected"]; ok {
		t.Fatal("disconnected runner QA channel is still registered")
	}
	if _, ok := hub.qaConn["qa_disconnected"]; ok {
		t.Fatal("disconnected runner QA route is still registered")
	}
	if hub.qaCh["qa_other"] != other || hub.qaConn["qa_other"] != 8 {
		t.Fatal("unrelated runner QA route was removed")
	}
}

func TestHubRoutesConcurrentQAByRequestID(t *testing.T) {
	hub := NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
	first := make(chan *QAEvent, 1)
	second := make(chan *QAEvent, 1)
	hub.qaCh["qa_first"] = first
	hub.qaConn["qa_first"] = 7
	hub.qaCh["qa_second"] = second
	hub.qaConn["qa_second"] = 7

	hub.handleQAEvent(&QAEvent{ID: "qa_first", EventType: "response", Chunk: "first"})
	hub.handleQAEvent(&QAEvent{ID: "qa_second", EventType: "response", Chunk: "second"})

	if event := <-first; event.Chunk != "first" {
		t.Fatalf("first route received %#v", event)
	}
	if event := <-second; event.Chunk != "second" {
		t.Fatalf("second route received %#v", event)
	}
}
