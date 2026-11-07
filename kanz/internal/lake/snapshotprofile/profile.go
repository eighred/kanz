// Package snapshotprofile owns only immutable opt-in broker profile data shared
// by the offline provisioner and the lake runtime. It opens no config or network.
package snapshotprofile

import "github.com/nats-io/nats.go/jetstream"

const Name = "LAKE_SNAPSHOT"
const Group = "lake-snapshot-v1"
const Durable = "lake-snapshot-v1-lake_snapshot_requested"
const SubjectRequested = "lake.snapshot.requested"
const SubjectCapturing = "lake.snapshot.capturing"
const SubjectPublished = "lake.snapshot.published"
const SubjectFailed = "lake.snapshot.failed"

func Stream() jetstream.StreamConfig {
	return jetstream.StreamConfig{Name: Name,
		Subjects:  []string{SubjectRequested, SubjectCapturing, SubjectPublished, SubjectFailed},
		Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, Discard: jetstream.DiscardNew,
		MaxBytes: 64 << 20, MaxMsgs: 10000, MaxMsgSize: 64 << 10, MaxMsgsPerSubject: -1,
		MaxAge: 0, MaxConsumers: 2, Replicas: 1,
	}
}
