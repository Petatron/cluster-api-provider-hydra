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

import "testing"

// Deletion has to reclaim both volumes Hydra created and destroy nothing else.
// Getting this wrong in either direction loses data: too narrow orphans a
// machine's cloud-init image, too broad destroys a volume an operator attached
// by hand, on an operation they asked for against a different object.
func TestPartitionOwnedDisks(t *testing.T) {
	const machine = "default-worker-1-0123456789abcdef0123"

	disks := []diskSource{
		{pool: testPool, volume: rootVolumeName(machine)},
		{pool: testPool, volume: cidataVolumeName(machine)},
		{pool: "operator-data", volume: "postgres-data.qcow2"},
		// Deliberately similar, and still not ours: another machine's disk must
		// not be reclaimed just because the names look alike.
		{pool: testPool, volume: rootVolumeName("default-worker-2-fedcba9876543210fedc")},
	}

	ours, foreign := partitionOwnedDisks(machine, disks)

	if len(ours) != 2 {
		t.Fatalf("reclaimable = %+v, want the root disk and the cloud-init image", ours)
	}
	got := map[string]bool{ours[0].volume: true, ours[1].volume: true}
	if !got[rootVolumeName(machine)] || !got[cidataVolumeName(machine)] {
		t.Errorf("reclaimable = %+v, want exactly the two volumes Create makes", ours)
	}

	if len(foreign) != 2 {
		t.Fatalf("left alone = %+v, want the operator volume and the other machine's disk", foreign)
	}
	for _, d := range foreign {
		if d.volume == rootVolumeName(machine) || d.volume == cidataVolumeName(machine) {
			t.Errorf("volume %q was classified as foreign but is ours", d.volume)
		}
	}
}

// A machine with no bootstrap data has one volume, and teardown must still
// reclaim it.
func TestPartitionOwnedDisksWithoutCloudInit(t *testing.T) {
	const machine = "default-worker-1-0123456789abcdef0123"

	ours, foreign := partitionOwnedDisks(machine, []diskSource{
		{pool: testPool, volume: rootVolumeName(machine)},
	})
	if len(ours) != 1 || ours[0].volume != rootVolumeName(machine) {
		t.Errorf("reclaimable = %+v, want the root disk", ours)
	}
	if len(foreign) != 0 {
		t.Errorf("left alone = %+v, want nothing", foreign)
	}
}
