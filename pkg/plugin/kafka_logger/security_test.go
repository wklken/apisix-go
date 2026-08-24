package kafka_logger

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSCRAMInvalidUTF8CredentialTerminates(t *testing.T) {
	const helperEnv = "KAFKA_LOGGER_SCRAM_INVALID_UTF8_HELPER"
	if os.Getenv(helperEnv) == "1" {
		runSCRAMInvalidUTF8Helper()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSCRAMInvalidUTF8CredentialTerminates$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatal("SCRAM construction with invalid UTF-8 credentials did not terminate within 30s")
	}
	if err != nil {
		t.Fatalf("helper subprocess error = %v\noutput:\n%s", err, output)
	}
	out := string(output)
	if !strings.Contains(out, "terminated error=") {
		t.Fatalf("helper output = %q, want terminated error marker", out)
	}
}

func runSCRAMInvalidUTF8Helper() {
	p := &Plugin{config: Config{
		Brokers: []Broker{{
			Host:       "127.0.0.1",
			Port:       9092,
			SASLConfig: &SASLConfig{Mechanism: "SCRAM-SHA-256", User: "\xf3\xcc\x80", Password: "password"},
		}},
		KafkaTopic: "apisix-logs",
	}}
	mechanism, err := p.saslMechanism(p.config.Brokers)
	if err != nil {
		fmt.Printf("terminated error=%v\n", err)
		os.Exit(0)
	}
	fmt.Printf("terminated mechanism=%s\n", mechanism.Name())
	os.Exit(0)
}
