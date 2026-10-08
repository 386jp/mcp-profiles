package listen

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireHeaders(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireHeaders(map[string]string{"Authorization": "Bearer t", "X-Key": "k"}, ok)

	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"all match", map[string]string{"Authorization": "Bearer t", "X-Key": "k"}, http.StatusOK},
		{"one wrong", map[string]string{"Authorization": "Bearer t", "X-Key": "x"}, http.StatusUnauthorized},
		{"one missing", map[string]string{"Authorization": "Bearer t"}, http.StatusUnauthorized},
		{"none", nil, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8000": true,
		"[::1]:8000":     true,
		"localhost:8000": true,
		"0.0.0.0:8000":   false,
		":8000":          false,
		"10.0.0.1:8000":  false,
	} {
		if got := isLoopback(addr); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}
