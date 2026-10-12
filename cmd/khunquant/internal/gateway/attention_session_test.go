package gateway

import (
	"context"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/attention"
	"github.com/cryptoquantumwave/khunquant/pkg/notify"
)

type capturingNotifier struct{ got []notify.Notification }

func (c *capturingNotifier) Deliver(_ context.Context, n notify.Notification) (notify.Result, error) {
	c.got = append(c.got, n)
	return notify.Result{Delivered: true}, nil
}

// The adapter hands the notify service the session form of a restricted message and
// nothing for any other: the session copy stays what it was.
func TestAttentionDeliverer_CarriesTheSessionBody(t *testing.T) {
	n := &capturingNotifier{}
	d := attentionDeliverer{svc: n}

	restricted := attention.Message{
		Title: "backup-done", Body: "> some text", SessionRestricted: true, SessionBody: attention.SessionWithheld,
	}
	plain := attention.Message{Title: "BTC crossed", Body: "detail"}
	for _, m := range []attention.Message{restricted, plain} {
		if ok, err := d.Deliver(context.Background(), m); err != nil || !ok {
			t.Fatalf("Deliver = %v, %v", ok, err)
		}
	}
	if len(n.got) != 2 {
		t.Fatalf("%d notifications", len(n.got))
	}
	if g := n.got[0]; g.Body != "> some text" || g.SessionBody == nil || *g.SessionBody != attention.SessionWithheld {
		t.Fatalf("restricted notification = %+v", g)
	}
	if g := n.got[1]; g.Body != "detail" || g.SessionBody != nil {
		t.Fatalf("plain notification = %+v", g)
	}
}
