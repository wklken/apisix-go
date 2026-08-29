package internal

import (
	"testing"

	"github.com/apache/rocketmq-client-go/v2/internal/remote"
)

func TestDefaultClientOptionsOwnsRemotingConfig(t *testing.T) {
	previous := remote.DefaultRemotingClientConfig.UseTls
	remote.DefaultRemotingClientConfig.UseTls = false
	t.Cleanup(func() { remote.DefaultRemotingClientConfig.UseTls = previous })

	first := DefaultClientOptions()
	second := DefaultClientOptions()
	if first.RemotingClientConfig == second.RemotingClientConfig {
		t.Fatal("default client options share one remoting config")
	}
	first.RemotingClientConfig.UseTls = true
	if second.RemotingClientConfig.UseTls {
		t.Fatal("changing one client remoting config changed another client")
	}
	if remote.DefaultRemotingClientConfig.UseTls {
		t.Fatal("changing one client remoting config changed the global default")
	}
}
