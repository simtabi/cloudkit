package cloudkit

import (
	"slices"
	"testing"
)

func TestCapabilityIsWrite(t *testing.T) {
	for _, c := range []Capability{CapInventory, CapFirewallRead, CapDNSRead, CapHostKeysRead, CapCostRead, CapManagedDatabasesRead, CapManagedDatabasesConnect} {
		if c.IsWrite() {
			t.Errorf("%s counts as a write", c)
		}
	}
	for _, c := range []Capability{CapServerCreate, CapServerResize, CapServerDestroy, CapServerPower, CapServerUpdate, CapFirewallWrite, CapNetworkWrite, CapVolumeWrite, CapSnapshotWrite, CapObjectStorageWrite, CapDNSWrite} {
		if !c.IsWrite() {
			t.Errorf("%s counts as a read", c)
		}
	}
	// Fail safe: a name this release does not know is a write.
	if !Capability("volume.read-ish").IsWrite() || !Capability("").IsWrite() {
		t.Error("an unknown capability counts as a read")
	}
}

func TestCapSet(t *testing.T) {
	var none CapSet
	if none.Get(CapInventory) != Unknown || none.Allowed(CapInventory) {
		t.Fatal("a nil CapSet is not all Unknown")
	}
	s := CapSet{}
	s.Set(CapServerCreate, Denied)
	s.Set(CapInventory, Allowed)
	if !s.Allowed(CapInventory) || s.Allowed(CapServerCreate) || s.Get(CapDNSWrite) != Unknown {
		t.Fatalf("CapSet = %v", s)
	}
	if got := s.Capabilities(); !slices.Equal(got, []Capability{CapInventory, CapServerCreate}) {
		t.Fatalf("Capabilities = %v", got)
	}
	for a, want := range map[Access]string{Unknown: "unknown", Allowed: "allowed", Denied: "denied", Access(9): "Access(9)"} {
		if a.String() != want {
			t.Errorf("%d.String() = %q", int(a), a.String())
		}
	}
	if Unknown != Access(0) {
		t.Fatal("the zero Access is not Unknown")
	}
}

func TestTokenModelString(t *testing.T) {
	for m, want := range map[TokenModel]string{Scoped: "scoped", ReadOrReadWrite: "read-or-read-write", PerProduct: "per-product", Unscoped: "unscoped", 0: "TokenModel(0)"} {
		if m.String() != want {
			t.Errorf("%d.String() = %q", int(m), m.String())
		}
	}
}

func TestDescriptorEgressIsACopy(t *testing.T) {
	d := fakeDescriptor()
	e := d.Egress()
	e.APIHosts[0] = "evil.test"
	if d.APIHosts[0] != fakeHost {
		t.Fatal("Egress shares the descriptor's slice")
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
}
