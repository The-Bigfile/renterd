package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	rhpv4 "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/renterd/v2/alerts"
	"go.sia.tech/renterd/v2/api"
	"go.sia.tech/renterd/v2/internal/test"
	"lukechampine.com/frand"
)

func TestMigrations(t *testing.T) {
	// configure the cluster to use one extra host
	rs := test.RedundancySettings
	cfg := test.AutopilotConfig
	cfg.Contracts.Amount = uint64(rs.TotalShards) + 1

	// create a new test cluster
	cluster := newTestCluster(t, testClusterOptions{
		autopilotConfig: &cfg,
		hosts:           int(cfg.Contracts.Amount),
	})
	defer cluster.Shutdown()

	// convenience variables
	b := cluster.Bus
	w := cluster.Worker
	tt := cluster.tt

	// create a helper to fetch used hosts
	usedHosts := func(key string) map[types.PublicKey]struct{} {
		res, _ := b.Object(context.Background(), testBucket, key, api.GetObjectOptions{})
		if res.Object == nil {
			t.Fatal("object not found")
		}
		used := make(map[types.PublicKey]struct{})
		for _, slab := range res.Object.Slabs {
			for _, sector := range slab.Shards {
				for hk := range sector.Contracts {
					used[hk] = struct{}{}
					break
				}
			}
		}
		return used
	}

	// add an object
	data := make([]byte, rhpv4.SectorSize)
	frand.Read(data)
	tt.OKAll(w.UploadObject(context.Background(), bytes.NewReader(data), testBucket, t.Name(), api.UploadObjectOptions{}))

	// assert amount of hosts used
	used := usedHosts(t.Name())
	if len(used) != test.RedundancySettings.TotalShards {
		t.Fatal("unexpected amount of hosts used", len(used), test.RedundancySettings.TotalShards)
	}

	// select one host to remove
	var removed types.PublicKey
	for _, h := range cluster.hosts {
		if _, ok := used[h.PublicKey()]; ok {
			cluster.RemoveHost(h)
			removed = h.PublicKey()
			break
		}
	}

	// assert we migrated away from the bad host
	tt.Retry(300, 100*time.Millisecond, func() error {
		if _, used := usedHosts(t.Name())[removed]; used {
			return errors.New("host is still used")
		}
		return nil
	})
	res, err := cluster.Bus.Object(context.Background(), testBucket, t.Name(), api.GetObjectOptions{})
	tt.OK(err)

	// check slabs
	shardHosts := 0
	for _, slab := range res.Object.Slabs {
		hosts := make(map[types.PublicKey]struct{})
		roots := make(map[types.Hash256]struct{})
		for _, shard := range slab.Shards {
			if len(shard.Contracts) == 0 {
				t.Fatal("each shard should have > 0 hosts")
			}
			for hpk, contracts := range shard.Contracts {
				if len(contracts) != 1 {
					t.Fatal("each host should have one contract")
				} else if _, found := hosts[hpk]; found {
					t.Fatal("each host should only be used once per slab")
				}
				hosts[hpk] = struct{}{}
			}
			roots[shard.Root] = struct{}{}
			shardHosts += len(shard.Contracts)
		}
	}

	// all shards should have 1 host except for 1. So we end up with 4 in total.
	if shardHosts != 4 {
		t.Fatalf("expected 4 shard hosts, got %v", shardHosts)
	}

	// create another bucket and add an object
	tt.OK(b.CreateBucket(context.Background(), "newbucket", api.CreateBucketOptions{}))
	tt.OKAll(w.UploadObject(context.Background(), bytes.NewReader(data), "newbucket", t.Name(), api.UploadObjectOptions{}))

	// assert we currently don't have any alerts
	ress, _ := b.Alerts(context.Background(), alerts.AlertsOpts{})
	if ress.Totals.Error+ress.Totals.Critical != 0 {
		t.Fatal("unexpected", ress)
	}

	// remove all hosts except for one to cause migrations to fail
	for len(cluster.hosts) > 1 {
		cluster.RemoveHost(cluster.hosts[0])
	}

	// fetch alerts and collect object ids until we found two
	var got map[string][]string
	tt.Retry(100, 100*time.Millisecond, func() error {
		got = make(map[string][]string)
		ress, err := b.Alerts(context.Background(), alerts.AlertsOpts{})
		tt.OK(err)
		for _, alert := range ress.Alerts {
			// skip if not a migration alert
			_, ok := alert.Data["objects"]
			if !ok {
				continue
			}

			// collect all object ids per bucket
			var objects []api.ObjectMetadata
			b, _ := json.Marshal(alert.Data["objects"])
			_ = json.Unmarshal(b, &objects)
			for _, object := range objects {
				got[object.Bucket] = append(got[object.Bucket], object.Key)
			}
		}
		if len(got) != 2 {
			return errors.New("unexpected number of buckets")
		}
		return nil
	})

	// assert we found our two objects across two buckets
	if want := map[string][]string{
		testBucket:  {fmt.Sprintf("/%s", t.Name())},
		"newbucket": {fmt.Sprintf("/%s", t.Name())},
	}; !reflect.DeepEqual(want, got) {
		t.Fatal("unexpected", cmp.Diff(want, got))
	}
}
