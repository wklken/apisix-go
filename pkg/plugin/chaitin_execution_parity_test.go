package plugin

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestParityChaitinHeadersOverrideUpstreamAtHeaderFilter(t *testing.T) {
	for _, mode := range []string{"block", "monitor"} {
		for _, prepared := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/prepared=%t", mode, prepared), func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				tasks := runtime.NewTaskRegistry(context.Background(), nil)
				owner, err := runtime.NewTaskOwner(tasks, "waf-test", runtime.TaskCore)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = listener.Close()
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if _, err := tasks.Stop(ctx); err != nil {
						t.Error(err)
					}
				})
				result := make(chan error, 1)
				err = owner.Go("respond", func(context.Context) error {
					connection, err := listener.Accept()
					if err != nil {
						result <- err
						return nil
					}
					defer func() { _ = connection.Close() }()
					_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
					for {
						var header [5]byte
						if _, err := io.ReadFull(connection, header[:]); err != nil {
							result <- err
							return nil
						}
						if _, err := io.CopyN(
							io.Discard,
							connection,
							int64(binary.LittleEndian.Uint32(header[1:])),
						); err != nil {
							result <- err
							return nil
						}
						if header[0]&0x80 != 0 {
							break
						}
					}
					for _, frame := range []struct {
						tag  byte
						body string
					}{{0x41, "."}, {0xa3, "X-WAF-Choice:approved\nSet-Cookie:waf-session=1\n"}} {
						var header [5]byte
						header[0] = frame.tag
						binary.LittleEndian.PutUint32(header[1:], uint32(len(frame.body)))
						if _, err := connection.Write(append(header[:], frame.body...)); err != nil {
							result <- err
							return nil
						}
					}
					result <- nil
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				address := listener.Addr().(*net.TCPAddr)
				binding := parityBinding(
					t,
					"chaitin-waf",
					fmt.Sprintf(`{"mode":%q,"nodes":[{"host":"127.0.0.1","port":%d}]}`, mode, address.Port),
					ScopeRoute,
				)
				next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("X-WAF-Choice", "upstream")
					w.Header().Set("Set-Cookie", "upstream-session=1")
					_, _ = w.Write([]byte("ok"))
				})
				handler := binding.Plugin.Handler(next)
				request := httptest.NewRequest("GET", "http://gateway.test/", nil)
				if prepared {
					plan, err := BuildResponsePlan(
						ResponsePlanInput{
							StaticBindings: []Binding{binding},
							BufferedConfig: base.BufferedResponseConfig{MaxBytes: base.DefaultBufferedResponseMaxBytes},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					handler = plan.Install(NewRequestPipeline([]Binding{binding}, nil), next)
					request, _ = apisixctx.EnsureRequestLifecycle(request, time.Now())
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				want := "approved"
				cookie := "waf-session=1"
				if mode == "monitor" {
					want = "upstream"
					cookie = "upstream-session=1"
				}
				if response.Code != 200 || response.Result().Header.Get("X-WAF-Choice") != want ||
					response.Result().Header.Get("Set-Cookie") != cookie ||
					response.Body.String() != "ok" {
					t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
				}
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("WAF fixture did not finish")
				}
			})
		}
	}
}
