package cloudkit

import (
	"fmt"
	"maps"
	"slices"
)

// Capability names one thing a token may be allowed to do, as "<area>.<verb>".
type Capability string

// The capabilities a probe reports on. Reads need no plan; every other one is
// a write and goes through [Plan] and [Registry.Apply].
const (
	CapInventory Capability = "inventory.read"

	CapServerCreate  Capability = "server.create"
	CapServerResize  Capability = "server.resize"
	CapServerDestroy Capability = "server.destroy"
	CapServerPower   Capability = "server.power"  // power and reboot
	CapServerUpdate  Capability = "server.update" // rename, labels, protection

	CapFirewallRead  Capability = "firewall.read"
	CapFirewallWrite Capability = "firewall.write"

	CapNetworkRead  Capability = "network.read"
	CapNetworkWrite Capability = "network.write"

	CapVolumeRead  Capability = "volume.read"
	CapVolumeWrite Capability = "volume.write"

	CapSnapshotRead  Capability = "snapshot.read"
	CapSnapshotWrite Capability = "snapshot.write"

	CapObjectStorageRead  Capability = "object-storage.read"
	CapObjectStorageWrite Capability = "object-storage.write"

	CapDNSRead  Capability = "dns.read"
	CapDNSWrite Capability = "dns.write"

	CapHostKeysRead Capability = "host-keys.read"
	CapCostRead     Capability = "cost.read"

	CapManagedDatabasesRead    Capability = "managed-databases.read"
	CapManagedDatabasesConnect Capability = "managed-databases.connect" // mints a short-lived connect credential; changes nothing
)

// reads lists the capabilities that change nothing. Anything else, including
// a capability this release does not know, counts as a write, so an unknown
// name fails safe.
var reads = []Capability{
	CapInventory, CapFirewallRead, CapNetworkRead, CapVolumeRead, CapSnapshotRead,
	CapObjectStorageRead, CapDNSRead, CapHostKeysRead, CapCostRead,
	CapManagedDatabasesRead, CapManagedDatabasesConnect,
}

// IsWrite reports whether c changes anything. An unknown capability is a write.
func (c Capability) IsWrite() bool { return !slices.Contains(reads, c) }

// Access is what a probe found for one capability.
type Access int

// The three answers a probe gives. The zero value is Unknown, so a capability
// a probe did not test is never mistaken for an allowed one.
const (
	Unknown Access = iota // not tested, or the provider would not say
	Allowed               // the token may do it
	Denied                // the token may not; neither product offers it
)

func (a Access) String() string {
	switch a {
	case Unknown:
		return "unknown"
	case Allowed:
		return "allowed"
	case Denied:
		return "denied"
	}
	return fmt.Sprintf("Access(%d)", int(a))
}

// CapSet is a probe's answer: each capability it tested and what it found.
// A capability missing from the set is Unknown. A nil CapSet reads as all
// Unknown; Set needs one made with make or a literal.
type CapSet map[Capability]Access

// Get returns what the probe found for c, Unknown when it did not say.
func (s CapSet) Get(c Capability) Access { return s[c] }

// Allowed reports whether the probe found c allowed. Unknown is not allowed.
func (s CapSet) Allowed(c Capability) bool { return s[c] == Allowed }

// Set records a for c.
func (s CapSet) Set(c Capability, a Access) { s[c] = a }

// Capabilities returns the capabilities in the set, sorted.
func (s CapSet) Capabilities() []Capability {
	return slices.Sorted(maps.Keys(s))
}
