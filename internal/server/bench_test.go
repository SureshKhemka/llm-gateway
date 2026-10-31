package server_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/SureshKhemka/llm-gateway/internal/testenv"
)

// BenchmarkChatNonStreaming measures the full HTTP path (gateway plus in-process
// simulator) per request.
func BenchmarkChatNonStreaming(b *testing.B) {
	env := testenv.New(b, testenv.Options{YAML: m1YAML, Env: map[string]string{"TEST_KEY_1": key1, "TEST_KEY_2": key2}})
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"Extract the order number from: order #42 shipped to Pune"}]}`)
	client := &http.Client{}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req, _ := http.NewRequest("POST", env.HTTP.URL+"/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+key1)
			resp, err := client.Do(req)
			if err != nil {
				b.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}
