package custody

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	accountingpb "github.com/eighred/kanz/kanz-schemas-go/accounting/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const BatchFormat = "kanz.synthetic-custody.v1"
const BatchLimit = 1 << 20

// The pilot retains at most 16 MiB of signed material per declared custody account.
const BatchRetentionLimit = 16 << 20
const BatchProducer = "custody-batch"
const BatchInbox = "_INBOX.custody_batch"

const batchStatementPrefix = "custody-batch.v1."

var errBatchReceipt = errors.New("custody batch: canonical statement differs from its authorized receipt")

func isBatchStatement(id string) bool { return strings.HasPrefix(id, batchStatementPrefix) }

var batchID = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$")

// BatchAuthority is an operator-controlled public verification policy. The signed
// file cannot introduce an account, instrument, tenant or authentication key.
type BatchAuthority struct {
	Source      string   `json:"source"`
	Tenant      string   `json:"tenant"`
	Portfolio   string   `json:"portfolio"`
	Custodian   string   `json:"custodian"`
	Account     string   `json:"account"`
	PublicKey   string   `json:"public_key"`
	Instruments []string `json:"instruments"`
	Currencies  []string `json:"currencies"`
}

type batchVerifier struct {
	authority BatchAuthority
	key       ed25519.PublicKey
}

func newBatchVerifier(a BatchAuthority, tenant string, pairs []string, accountDeclaration string) (batchVerifier, error) {
	for _, id := range []string{a.Source, a.Tenant, a.Portfolio, a.Custodian, a.Account} {
		if !batchID.MatchString(id) {
			return batchVerifier{}, errors.New("custody batch: invalid source binding")
		}
	}
	if a.Tenant != tenant || !slices.Contains(pairs, a.Portfolio+":"+a.Custodian) {
		return batchVerifier{}, errors.New("custody batch: tenant or reconciliation pair is unauthorized")
	}
	declared, err := ParseCustodyAccounts(accountDeclaration)
	if err != nil {
		return batchVerifier{}, err
	}
	if !slices.Equal(declared[a.Portfolio][a.Custodian], []string{a.Account}) {
		return batchVerifier{}, errors.New("custody batch: source must cover the entire declared single-account pair")
	}
	for portfolio, custodians := range declared {
		for custodian, accounts := range custodians {
			if (portfolio != a.Portfolio || custodian != a.Custodian) && slices.Contains(accounts, a.Account) {
				return batchVerifier{}, errors.New("custody batch: account has another declared owner")
			}
		}
	}
	key, err := base64.StdEncoding.Strict().DecodeString(a.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return batchVerifier{}, errors.New("custody batch: invalid public key")
	}
	for _, ids := range [][]string{a.Instruments, a.Currencies} {
		for _, id := range ids {
			if !batchID.MatchString(id) {
				return batchVerifier{}, errors.New("custody batch: invalid reference mapping")
			}
		}
	}
	a.Instruments = slices.Clone(a.Instruments)
	a.Currencies = slices.Clone(a.Currencies)
	return batchVerifier{a, key}, nil
}

type signedBatch struct {
	Format    string          `json:"format"`
	Source    string          `json:"source"`
	Tenant    string          `json:"tenant"`
	Account   string          `json:"account"`
	Revision  int64           `json:"revision"`
	Previous  string          `json:"previous_sha256"`
	Complete  bool            `json:"complete"`
	Statement json.RawMessage `json:"statement"`
}

type batchInput struct {
	header    signedBatch
	digest    string
	statement *accountingpb.CustodianStatement
}

// The detached signature covers the exact file bytes. The outer object's compact,
// fixed field order prevents duplicate keys, unknown fields and parser ambiguity.
// Protobuf JSON performs the independent strict decode of the existing payload.
func (v batchVerifier) verify(raw, signature []byte) (batchInput, error) {
	if len(raw) == 0 || len(raw) > BatchLimit || !ed25519.Verify(v.key, raw, signature) {
		return batchInput{}, errors.New("custody batch: invalid signed input")
	}
	var h signedBatch
	if err := json.Unmarshal(raw, &h); err != nil {
		return batchInput{}, errors.New("custody batch: invalid batch JSON")
	}
	canonical, err := json.Marshal(h)
	if err != nil || !bytes.Equal(bytes.TrimSuffix(raw, []byte("\n")), canonical) {
		return batchInput{}, errors.New("custody batch: noncanonical batch JSON")
	}
	a := v.authority
	if h.Format != BatchFormat || h.Source != a.Source || h.Tenant != a.Tenant || h.Account != a.Account || !h.Complete || h.Revision <= 0 {
		return batchInput{}, errors.New("custody batch: invalid version, source scope or completeness")
	}
	if h.Revision == 1 && h.Previous != "" || h.Revision > 1 && !isBatchDigest(h.Previous) {
		return batchInput{}, errors.New("custody batch: invalid preceding revision reference")
	}
	var presence map[string]json.RawMessage
	if err := json.Unmarshal(h.Statement, &presence); err != nil {
		return batchInput{}, errors.New("custody batch: invalid statement")
	}
	for _, field := range []string{"positions", "cash", "transactions"} {
		if b := presence[field]; len(b) == 0 || b[0] != '[' {
			return batchInput{}, errors.New("custody batch: explicitly declare all three statement inventories")
		}
	}
	msg := new(accountingpb.CustodianStatement)
	if err := protojson.Unmarshal(h.Statement, msg); err != nil {
		return batchInput{}, errors.New("custody batch: invalid protobuf JSON")
	}
	if msg.GetPortfolioId() != a.Portfolio || msg.GetCustodianId() != a.Custodian || msg.GetBusinessDate() == nil || msg.BusinessDate.CheckValid() != nil || !BusinessDay(msg.BusinessDate.AsTime()).Equal(msg.BusinessDate.AsTime()) || msg.GetStatementId() != "" || msg.GetReceivedAt() != nil {
		return batchInput{}, errors.New("custody batch: invalid statement ownership, date or reserved receipt fields")
	}
	if msg.Grain != accountingpb.StatementGrain_STATEMENT_GRAIN_TRANSACTIONS && msg.Grain != accountingpb.StatementGrain_STATEMENT_GRAIN_BALANCES_ONLY {
		return batchInput{}, errors.New("custody batch: an explicit supported grain is required")
	}
	for _, p := range msg.Positions {
		if !slices.Contains(a.Instruments, p.GetInstrumentId()) {
			return batchInput{}, errors.New("custody batch: unrecognized position instrument")
		}
	}
	for _, c := range msg.Cash {
		if !slices.Contains(a.Currencies, c.GetCurrencyCode()) {
			return batchInput{}, errors.New("custody batch: unrecognized cash currency")
		}
	}
	for _, t := range msg.Transactions {
		if !slices.Contains(a.Instruments, t.GetInstrumentId()) || !slices.Contains(a.Currencies, t.GetCurrencyCode()) || t.GetTradeDate() == nil || t.TradeDate.CheckValid() != nil || !BusinessDay(t.TradeDate.AsTime()).Equal(msg.BusinessDate.AsTime()) || t.GetQuantity() == nil || t.GetPrice() == nil || t.GetCash() == nil || strings.TrimSpace(t.GetExternalRef()) != t.GetExternalRef() {
			return batchInput{}, errors.New("custody batch: transaction lacks complete in-scope execution evidence")
		}
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	msg.StatementId = batchStatementID(a.Source, h.Revision, digest)
	return batchInput{h, digest, msg}, nil
}

func batchStatementID(source string, revision int64, digest string) string {
	return fmt.Sprintf("%s%s.r%d.%s", batchStatementPrefix, source, revision, digest)
}

func isBatchDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func (b batchInput) stamp(received time.Time) error {
	if received.Before(b.statement.BusinessDate.AsTime().AddDate(0, 0, 1)) {
		return errors.New("custody batch: business date has not closed")
	}
	b.statement.ReceivedAt = timestamppb.New(received)
	_, err := statementFromProto(b.statement)
	return err
}
