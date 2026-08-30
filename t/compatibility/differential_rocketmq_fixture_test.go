package pluginintegration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/producer"
)

func TestReadRocketMQCommandAcceptsScalarExtFieldsFromAPISIX(t *testing.T) {
	header := []byte(
		`{"code":10,"language":"JAVA","version":433,"opaque":7,"flag":0,"extFields":{"topic":"test2","bornHostV6Flag":false,"queueId":0}}`,
	)
	var frame bytes.Buffer
	if err := binary.Write(&frame, binary.BigEndian, int32(4+len(header))); err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(&frame, binary.BigEndian, uint32(len(header))); err != nil {
		t.Fatal(err)
	}
	if _, err := frame.Write(header); err != nil {
		t.Fatal(err)
	}
	command, err := readRocketMQCommand(&frame)
	if err != nil {
		t.Fatalf("decode APISIX RocketMQ header: %v", err)
	}
	if command.ExtFields["topic"] != "test2" ||
		command.ExtFields["bornHostV6Flag"] != "false" || command.ExtFields["queueId"] != "0" {
		t.Fatalf("extFields = %#v", command.ExtFields)
	}
}

func TestDifferentialRocketMQFixtureCapturesAPISIXSendMessageV2(t *testing.T) {
	fixture := &differentialFixtureServer{}
	_, captured, err := fixture.differentialRocketMQResponse(&rocketMQCommand{
		Code: rocketMQSendMessageV2Code,
		ExtFields: rocketMQExtFields{
			"b": "test2", "e": "0", "i": "KEYS\x01key1\x02TAGS\x01tag1\x02",
		},
		Body: []byte(`{"x_ip":"127.0.0.1"}`),
	}, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if captured == nil || captured.Method != differentialRocketMQMethod ||
		captured.Path != "test2" || captured.Host != "key1" ||
		captured.Headers.Get(differentialRocketMQTagHeader) != "tag1" ||
		captured.Headers.Get(differentialRocketMQQueueIDHeader) != "0" ||
		captured.Body != `{"x_ip":"127.0.0.1"}` {
		t.Fatalf("captured APISIX SEND_MESSAGE_V2 = %#v", captured)
	}
}

func TestDifferentialHTTPRocketMQFixtureCapturesOnlyOriginAndPublishedMessage(t *testing.T) {
	spec := differentialCasesForPlugin("rocketmq-logger")[0].Fixture
	fixture, err := startDifferentialHTTPRocketMQFixture(spec)
	if err != nil {
		t.Fatalf("start fixture: %v", err)
	}
	defer fixture.close()
	fixture.reset()

	request, err := http.NewRequest(
		http.MethodGet,
		"http://127.0.0.1:"+strconv.Itoa(fixture.port())+"/hello",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "differential.example.test"
	response, err := (&http.Client{Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatalf("origin request: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close origin response: %v / %v", readErr, closeErr)
	}
	if response.StatusCode != http.StatusOK || string(body) != "hello world\n" {
		t.Fatalf("origin response = %d %q", response.StatusCode, body)
	}

	client, err := rocketmq.NewProducer(
		producer.WithNameServer([]string{fixture.listener.Addr().String()}),
		producer.WithSendMsgTimeout(2*time.Second),
		producer.WithInstanceName("apisix-go-differential-rocketmq-fixture"),
	)
	if err != nil {
		t.Fatalf("new RocketMQ producer: %v", err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start RocketMQ producer: %v", err)
	}
	defer func() { _ = client.Shutdown() }()

	message := primitive.NewMessage("test2", []byte(`{"x_ip":"127.0.0.1"}`))
	message.WithKeys([]string{"key1"})
	message.WithTag("tag1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.SendSync(ctx, message); err != nil {
		t.Fatalf("publish RocketMQ message: %v", err)
	}

	calls, err := fixture.collectWithTimeout(2, 3*time.Second)
	if err != nil {
		t.Fatalf("collect fixture calls: %v", err)
	}
	if calls[0].Method != http.MethodGet || calls[0].Path != "/hello" ||
		calls[0].Host != "differential.example.test" || calls[0].Body != "" {
		t.Fatalf("origin call = %#v", calls[0])
	}
	if calls[1].Method != differentialRocketMQMethod || calls[1].Path != "test2" ||
		calls[1].Host != "key1" || calls[1].Body != `{"x_ip":"127.0.0.1"}` ||
		calls[1].Headers.Get(differentialRocketMQTagHeader) != "tag1" ||
		calls[1].Headers.Get(differentialRocketMQQueueIDHeader) != "0" {
		t.Fatalf("RocketMQ call = %#v", calls[1])
	}
}

func TestDifferentialRocketMQRouteAdvertisesReachableSingleBroker(t *testing.T) {
	body, err := differentialRocketMQRouteBody("127.0.0.1", 19876)
	if err != nil {
		t.Fatal(err)
	}
	var route struct {
		QueueDatas []struct {
			BrokerName     string `json:"brokerName"`
			ReadQueueNums  int    `json:"readQueueNums"`
			WriteQueueNums int    `json:"writeQueueNums"`
		} `json:"queueDatas"`
		BrokerDatas []struct {
			BrokerName  string            `json:"brokerName"`
			BrokerAddrs map[string]string `json:"brokerAddrs"`
		} `json:"brokerDatas"`
	}
	if err := json.Unmarshal(body, &route); err != nil {
		t.Fatalf("decode route: %v", err)
	}
	if len(route.QueueDatas) != 1 || route.QueueDatas[0].BrokerName != "differential-broker" ||
		route.QueueDatas[0].ReadQueueNums != 1 || route.QueueDatas[0].WriteQueueNums != 1 ||
		len(route.BrokerDatas) != 1 || route.BrokerDatas[0].BrokerName != "differential-broker" ||
		route.BrokerDatas[0].BrokerAddrs["0"] != "127.0.0.1:19876" {
		t.Fatalf("route = %#v", route)
	}
}

func TestDifferentialRocketMQAdvertisedHostUsesExecutionSide(t *testing.T) {
	t.Setenv(differentialHostGatewayEnv, "192.168.127.254")
	tests := []struct {
		name       string
		remote     net.Addr
		oracleSide bool
		want       string
	}{
		{name: "candidate loopback", remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 3000}, want: "127.0.0.1"},
		{
			name:   "oracle bridge",
			remote: &net.TCPAddr{IP: net.ParseIP("192.168.127.2"), Port: 3000},
			want:   "192.168.127.254",
		},
		{
			name:       "oracle loopback forwarder",
			remote:     &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 3000},
			oracleSide: true,
			want:       "192.168.127.254",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := differentialRocketMQAdvertisedHost(test.remote, test.oracleSide); got != test.want {
				t.Fatalf("advertised host = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDifferentialRocketMQFixtureRejectsNonPinnedContract(t *testing.T) {
	spec := differentialCasesForPlugin("rocketmq-logger")[0].Fixture
	spec.ExpectedCalls = 1
	fixture, err := startDifferentialHTTPRocketMQFixture(spec)
	if fixture != nil || err == nil || !strings.Contains(err.Error(), "exactly two") {
		t.Fatalf("fixture/error = %#v / %v", fixture, err)
	}
}
