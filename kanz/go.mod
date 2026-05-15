module github.com/kanz-eng/kanz

go 1.23

require (
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.7.1
	github.com/kanz-eng/kanz-schemas-go v0.0.0-00010101000000-000000000000
	github.com/nats-io/nats.go v1.39.0
	github.com/segmentio/kafka-go v0.4.47
	google.golang.org/protobuf v1.36.11
)

// Local replace until the first kanz-schemas release. After `cd
// ../kanz-schemas && buf generate && (cd gen/go && go mod init
// github.com/kanz-eng/kanz-schemas-go && go mod tidy)`, this resolves
// against the locally-generated SDK. Removed once a tagged release exists.
replace github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go
