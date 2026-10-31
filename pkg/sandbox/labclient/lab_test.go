package labclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientLab_ReadsGroupsAndKinds(t *testing.T) {
	body := `{"groups":[{"name":"chat","description":"d","ops":[{"name":"send","description":"s"}]}],"kinds":["chat.out","event.*"]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/api/sandbox/lab" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Lab(context.Background())
	if err != nil || len(info.Groups) != 1 || info.Groups[0].Ops[0].Name != "send" || len(info.Kinds) != 2 {
		t.Fatalf("Lab = %+v, %v", info, err)
	}
	if gs, err := c.Groups(context.Background()); err != nil || len(gs) != 1 {
		t.Fatalf("Groups = %v, %v", gs, err)
	}

	body = `{"groups":[]}` // an instance that predates kinds: unknown, not "none"
	if info, err := c.Lab(context.Background()); err != nil || info.Kinds != nil {
		t.Fatalf("Lab without kinds = %+v, %v; want Kinds nil", info, err)
	}
}
