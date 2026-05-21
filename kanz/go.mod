module github.com/kanz-eng/kanz

go 1.26.1

require (
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.7.1
	github.com/kanz-eng/kanz-schemas-go v0.0.0-00010101000000-000000000000
	github.com/nats-io/nats.go v1.39.0
	github.com/segmentio/kafka-go v0.4.47
	google.golang.org/grpc v1.81.1
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/nats-io/nkeys v0.4.9 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/pierrec/lz4/v4 v4.1.15 // indirect
	golang.org/x/crypto v0.48.0 // indirect
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.42.0 // indirect
	golang.org/x/text v0.34.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260226221140-a57be14db171 // indirect
)

// Local replace until the first kanz-schemas release. After `cd
// ../kanz-schemas && buf generate && (cd gen/go && go mod init
// github.com/kanz-eng/kanz-schemas-go && go mod tidy)`, this resolves
// against the locally-generated SDK. Removed once a tagged release exists.
replace github.com/kanz-eng/kanz-schemas-go => ../kanz-schemas/gen/go
