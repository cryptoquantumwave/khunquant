package newsstore_test

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/events"
	"github.com/cryptoquantumwave/khunquant/pkg/perception"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/newsstore"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/perceptiontest"
	"github.com/cryptoquantumwave/khunquant/pkg/perception/sources"
	"github.com/cryptoquantumwave/khunquant/pkg/sqlitex/sqlitextest"
)

var now = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

func TestStore_SeenMarkPrune(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	s, err := newsstore.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Seen(ctx, "abc"); err != nil || ok {
		t.Fatalf("Seen on an empty table = %v, %v", ok, err)
	}
	if err := s.Mark(ctx, "abc", now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Mark(ctx, "def", now); err != nil {
		t.Fatal(err)
	}
	if err := s.Mark(ctx, "def", now); err != nil { // marking again is not an error
		t.Fatal(err)
	}
	if err := s.Prune(ctx, now.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Seen(ctx, "abc"); ok {
		t.Error("an entry older than 7 days survived the housekeeping")
	}
	if ok, _ := s.Seen(ctx, "def"); !ok {
		t.Error("a fresh entry was pruned")
	}
	// The table is the one the spec names: news_seen(url_hash PRIMARY KEY, at_ms), migration v1 of component news.
	var comp string
	var ver int
	if err := db.QueryRow(`SELECT component, MAX(version) FROM schema_migrations WHERE component = 'news'`).Scan(&comp, &ver); err != nil || ver != 1 {
		t.Errorf("migration = %q v%d (%v)", comp, ver, err)
	}
	if _, err := db.Exec(`INSERT INTO news_seen (url_hash, at_ms) VALUES ('def', 1)`); err == nil {
		t.Error("url_hash is not a primary key")
	}
}

func TestStore_ParseCounts(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	s, _ := newsstore.New(ctx, db)
	for i := 0; i < 3; i++ {
		if err := s.AddParse(ctx, "2026-10-19"); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.Parses(ctx, "2026-10-19"); n != 3 {
		t.Errorf("parses = %d", n)
	}
	if n, _ := s.Parses(ctx, "2026-10-20"); n != 0 {
		t.Errorf("another day = %d", n)
	}
	// A second store over the same database (a restart) sees the same count.
	again, _ := newsstore.New(ctx, db)
	if n, _ := again.Parses(ctx, "2026-10-19"); n != 3 {
		t.Errorf("after a restart = %d", n)
	}
}

type out struct{ n atomic.Int32 }

func (o *out) Emit(events.Event)                 { o.n.Add(1) }
func (o *out) EmitLatest(events.Event)           { o.n.Add(1) }
func (o *out) EmitNonBlocking(events.Event) bool { o.n.Add(1); return true }

type search struct{}

func (search) Search(context.Context, string, int) ([]string, error) {
	return []string{"https://news.example.com/a", "https://other.org/b"}, nil
}

type fetch struct{ calls atomic.Int32 }

func (f *fetch) Fetch(context.Context, string, int) (string, error) {
	f.calls.Add(1)
	return "an article", nil
}

type llm struct{ calls atomic.Int32 }

func (l *llm) Complete(context.Context, string, string, int) (string, error) {
	l.calls.Add(1)
	return `{"asset":"BTC","relevant":false}`, nil
}

// An article is parsed once across a restart: the second source, built over the same
// database, finds both URLs already read and the day's count already spent.
func TestNews_DedupAcrossRestart(t *testing.T) {
	db := sqlitextest.Open(t)
	ctx := context.Background()
	f, l := &fetch{}, &llm{}
	build := func() *sources.News {
		store, err := newsstore.New(ctx, db) // a fresh handle, as after a restart
		if err != nil {
			t.Fatal(err)
		}
		return sources.NewNews(sources.NewsOptions{
			Search: search{}, Fetch: f, Complete: l, Seen: store, Clock: perceptiontest.NewClock(now),
			Assets: func(context.Context) []sources.NewsAsset {
				return []sources.NewsAsset{{Symbol: "BTC", Weight: 1, Held: true}}
			},
			MaxParsesPerRun: 10, MaxParsesPerDay: 10,
		})
	}
	var o out
	build().Run(ctx, &o)
	if f.calls.Load() != 2 || l.calls.Load() != 2 {
		t.Fatalf("first run: %d fetches, %d parses, want 2 and 2", f.calls.Load(), l.calls.Load())
	}
	restarted := build()
	restarted.Run(ctx, &o)
	if f.calls.Load() != 2 || l.calls.Load() != 2 {
		t.Errorf("after a restart: %d fetches, %d parses, want no new ones", f.calls.Load(), l.calls.Load())
	}
	if restarted.Stats().Seen != 2 {
		t.Errorf("seen = %d", restarted.Stats().Seen)
	}

	// The key is the sha1 of the URL, as the spec says.
	sum := sha1.Sum([]byte("https://news.example.com/a"))
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM news_seen WHERE url_hash = ?`, hex.EncodeToString(sum[:])).Scan(&n); err != nil || n != 1 {
		t.Errorf("sha1 key rows = %d (%v)", n, err)
	}
	if _, err := newsstore.New(ctx, nil); err == nil {
		t.Error("a nil database must be refused")
	}
	var _ perception.Source = restarted
}
