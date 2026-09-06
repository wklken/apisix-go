package kafka_proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestPubSubErrorUsesZeroCodeAndHandlerMessage(t *testing.T) {
	for _, test := range []struct {
		name    string
		command PubSubCommand
		cause   error
		want    string
	}{
		{"list-offset", CmdKafkaListOffset, errors.New("broker unavailable"), "failed to list offset, topic: orders, partition: 2, err: broker unavailable"},
		{"fetch-timeout", CmdKafkaFetch, context.DeadlineExceeded, "failed to fetch message, topic: orders, partition: 2, err: context deadline exceeded"},
		{"unregistered-empty", CmdEmpty, nil, "unknown command"},
	} {
		t.Run(test.name, func(t *testing.T) {
			served := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				served <- ServePubSubWebSocket(w, r, []string{"kafka://127.0.0.1:9092"}, TransportOptions{}, func(context.Context, []string, ConsumerOptions) (KafkaConsumer, error) {
					return &fakeKafkaConsumer{listErr: test.cause, fetchErr: test.cause}, nil
				})
			}))
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = conn.Close()
				if err := <-served; err != nil {
					t.Error(err)
				}
			}()
			wire := mustMarshalPubSubRequest(
				t,
				PubSubRequest{Sequence: 7, Command: test.command, Topic: "orders", Partition: 2},
			)
			if err := conn.WriteMessage(websocket.BinaryMessage, wire); err != nil {
				t.Fatal(err)
			}
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			response, err := ParsePubSubResponse(data)
			if err != nil {
				t.Fatal(err)
			}
			if response.Kind != RespError || response.Sequence != 7 || response.Code != 0 ||
				response.Message != test.want {
				t.Fatalf("error response=%+v, want sequence7/code0/message %q", response, test.want)
			}
		})
	}
}
