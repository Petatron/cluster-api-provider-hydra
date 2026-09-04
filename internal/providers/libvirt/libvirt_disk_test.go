/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package libvirt

import (
	"path/filepath"
	"testing"
)

// Deletion has to reclaim both volumes Hydra created and destroy nothing else.
// Getting this wrong in either direction loses data: too narrow orphans a
// machine's cloud-init image, too broad destroys a volume an operator attached
// by hand, on an operation they asked for against a different object.
func TestPartitionOwnedDisks(t *testing.T) {
	const machine = "default-worker-1-0123456789abcdef0123"

	const dir = "/var/lib/libvirt/k8s-workers/"

	disks := []diskSource{
		{path: dir + rootVolumeName(machine)},
		{path: dir + cidataVolumeName(machine)},
		{path: "/srv/operator-data/postgres-data.qcow2"},
		// Deliberately similar, and still not ours: another machine's disk must
		// not be reclaimed just because the names look alike.
		{path: dir + rootVolumeName("default-worker-2-fedcba9876543210fedc")},
		// Ours, in a different directory. Ownership follows the name Create
		// chose, not where the pool happened to put it -- a machine built before
		// the pool was redefined still has to be reclaimable.
		{path: "/mnt/bigger-disk/" + rootVolumeName(machine)},
	}

	ours, foreign := partitionOwnedDisks(machine, disks)

	if len(ours) != 3 {
		t.Fatalf("reclaimable = %+v, want both volumes plus the relocated root disk", ours)
	}
	got := map[string]bool{}
	for _, d := range ours {
		got[filepath.Base(d.path)] = true
	}
	if !got[rootVolumeName(machine)] || !got[cidataVolumeName(machine)] {
		t.Errorf("reclaimable = %+v, want exactly the volumes Create makes", ours)
	}

	if len(foreign) != 2 {
		t.Fatalf("left alone = %+v, want the operator volume and the other machine's disk", foreign)
	}
	for _, d := range foreign {
		if base := filepath.Base(d.path); base == rootVolumeName(machine) || base == cidataVolumeName(machine) {
			t.Errorf("volume %q was classified as foreign but is ours", d.path)
		}
	}
}

// A machine with no bootstrap data has one volume, and teardown must still
// reclaim it.
func TestPartitionOwnedDisksWithoutCloudInit(t *testing.T) {
	const machine = "default-worker-1-0123456789abcdef0123"

	root := "/var/lib/libvirt/k8s-workers/" + rootVolumeName(machine)
	ours, foreign := partitionOwnedDisks(machine, []diskSource{{path: root}})
	if len(ours) != 1 || ours[0].path != root {
		t.Errorf("reclaimable = %+v, want the root disk", ours)
	}
	if len(foreign) != 0 {
		t.Errorf("left alone = %+v, want nothing", foreign)
	}
}
