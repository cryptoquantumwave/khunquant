package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cryptoquantumwave/khunquant/pkg/config"
	"github.com/cryptoquantumwave/khunquant/pkg/sandbox"
)

// The sandbox profile is what `khunquant sandbox up` hands a locked gateway. It must
// survive config.LoadConfig and already satisfy enforceLockedConfig: the overlay
// then has nothing to force, apart from the management token it sets from the lock.
func TestProfile_Invariants(t *testing.T) {
	home := newInstanceHome(t)
	t.Setenv("KHUNQUANT_HOME", home)
	t.Setenv("KHUNQUANT_CONFIG", filepath.Join(home, "config.json"))
	t.Setenv("KQ_SANDBOX_TOKEN", "") // env.Parse would copy it into debug.sandbox.token

	path, err := sandbox.WriteProfile(sandbox.ProfileOptions{Home: home, Port: 18793})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(home, "config.json") {
		t.Fatalf("WriteProfile path = %q", path)
	}

	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	enforced, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	lk := sandbox.LockOptions{InstanceName: "profile", Token: lockTestToken}
	// The lock must be active for CheckLockedModel to judge the model.
	lockProcessForTest(t, home)
	if err := enforceLockedConfig(enforced, lk); err != nil {
		t.Fatalf("the profile does not satisfy enforceLockedConfig: %v", err)
	}

	// enforceLockedConfig sets the management token from the lock and clears the
	// legacy providers; everything else it forces must already hold.
	if enforced.Gateway.ManagementToken != lk.Token {
		t.Errorf("management token not set by the overlay")
	}
	loaded.Gateway.ManagementToken = lk.Token
	loaded.Providers = config.ProvidersConfig{}
	loaded.Tools.MCP.Servers = nil // the overlay nils the map; an empty map is the same
	if !reflect.DeepEqual(loaded, enforced) {
		lj, _ := json.Marshal(loaded)
		ej, _ := json.Marshal(enforced)
		t.Errorf("enforceLockedConfig changed the profile:\n loaded   %s\n enforced %s", lj, ej)
	}

	// Nothing on disk carries a token, and the profile enables no real channel.
	for _, name := range []string{"config.json", ".security.yml"} {
		b, err := os.ReadFile(filepath.Join(home, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(b), lockTestToken) {
			t.Errorf("%s contains the instance token", name)
		}
	}
	if loaded.Channels.Pico.Enabled || loaded.Channels.Telegram.Enabled {
		t.Error("a real channel is enabled in the profile")
	}
}
