package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/internal/refdata"
	"github.com/eighred/kanz/pkg/auth"
)

const sectorDay = "2020-02-03"
const nextSectorDay = "2020-02-04"
const sectorTenant = "basis-test"

var sectorInstruments = []string{"SYNTH-A", "SYNTH-B", "SYNTH-C"}

type masterPilot struct {
	storeURL, historyAddr, liveAddr, drop string
	source                                *refdata.SectorHistory
	stop                                  func()
}

func loopbackAddress(t *testing.T) string {
	t.Helper()
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = socket.Close() }()
	return socket.Addr().String()
}

func startMasterPilot(t *testing.T, ctx context.Context) *masterPilot {
	t.Helper()
	p := &masterPilot{
		storeURL: os.Getenv("TEST_RETURN_BASIS_DATAMASTER"), historyAddr: loopbackAddress(t),
		liveAddr: loopbackAddress(t), drop: filepath.Join(t.TempDir(), "reference.csv"),
	}
	var err error
	p.source, err = refdata.NewSectorHistory("http://"+p.historyAddr, sectorTenant, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.write(t, 1, "CURRENT")
	p.launch(t, ctx)
	t.Cleanup(p.shutdown)
	return p
}

func (p *masterPilot) launch(t *testing.T, ctx context.Context) {
	t.Helper()
	worker, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(worker, os.Getenv("RETURN_PILOT_DATAMASTER_BIN"))
	cmd.Env = []string{
		"DATAMASTER_DATABASE_URL=" + p.storeURL, "DATAMASTER_TENANT=" + sectorTenant,
		"DATAMASTER_LISTEN=" + p.liveAddr, "DATAMASTER_SECTOR_HISTORY_LISTEN=" + p.historyAddr,
		"DATAMASTER_REF_FILES=SYNTH=" + p.drop, "DATAMASTER_VENDOR_PRIORITY=SYNTH=0", "DATAMASTER_REFRESH_INTERVAL=250ms",
	}
	for _, key := range []string{"SystemRoot", "TEMP", "TMP", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	if p.storeURL == "" || os.Getenv("RETURN_PILOT_DATAMASTER_BIN") == "" || cmd.Start() != nil {
		cancel()
		t.Fatal("start the normal Datamaster binary with a restricted PostgreSQL role")
	}
	p.stop = func() { cancel(); _ = cmd.Wait() }
}

func (p *masterPilot) shutdown() {
	if p.stop != nil {
		p.stop()
		p.stop = nil
	}
}

func (p *masterPilot) write(t *testing.T, edition int, current string) {
	t.Helper()
	taxonomy, classification := "SYNTH", "GROWTH"
	if edition == 2 {
		classification = "VALUE"
	} else if edition == 3 {
		taxonomy, classification = "", ""
	}
	contents := "symbol,asset_class,currency,sector_taxonomy,sector_code,as_of,sector_effective_date\n" +
		"SYNTH-C,EQUITY,USD,SYNTH," + current + ",2020-02-05T00:00:00Z,\n" +
		"SYNTH-A,,,SYNTH,GROWTH,2020-02-05T00:00:00Z," + sectorDay + "\n" +
		"SYNTH-B,,,SYNTH,VALUE,2020-02-05T00:00:00Z," + sectorDay + "\n" +
		"SYNTH-C,,,SYNTH,FINANCE,2020-02-05T00:00:00Z," + nextSectorDay + "\n" +
		fmt.Sprintf("SYNTH-C,,,%s,%s,2020-02-%02dT00:00:00Z,%s\n", taxonomy, classification, 4+edition, sectorDay)
	if edition > 1 {
		contents += "SYNTH-C,,,SYNTH,GROWTH,2020-02-05T00:00:00Z," + sectorDay + "\n"
	}
	pending := p.drop + ".pending"
	if err := os.WriteFile(pending, []byte(contents), 0600); err != nil {
		t.Fatal("write synthetic vendor extract")
	}
	if err := os.Rename(pending, p.drop); err != nil {
		t.Fatal("deliver complete synthetic vendor extract")
	}
}

func (p *masterPilot) awaitEdition(t *testing.T, wait func(string, func(context.Context) bool), code string) (time.Time, []refdata.DatedSector) {
	t.Helper()
	var asOf time.Time
	var rows []refdata.DatedSector
	wait("Datamaster did not master the dated vendor edition", func(ctx context.Context) bool {
		asOf = time.Now().UTC().Truncate(time.Microsecond)
		var err error
		rows, err = p.source.At(ctx, sectorTenant, sectorDay, asOf, sectorInstruments)
		return err == nil && len(rows) == 3 && rows[2].InstrumentID == "SYNTH-C" && rows[2].Code == code
	})
	return asOf, rows
}

func (p *masterPilot) verifyRetained(t *testing.T, ctx context.Context, wait func(string, func(context.Context) bool), oldHorizon, correctedHorizon time.Time,
	original, corrected, next []refdata.DatedSector,
) {
	t.Helper()
	p.write(t, 1, "CURRENT_REPLAY")
	wait("Datamaster did not reread the earlier mounted extract", func(check context.Context) bool {
		req, _ := http.NewRequestWithContext(check, http.MethodGet, "http://"+p.liveAddr+"/v1/securities/SYNTH-C", nil)
		auth.SetPrincipalHeaders(req.Header, "gateway-fixture", sectorTenant, nil)
		resp, err := (&http.Client{Timeout: time.Second}).Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		var body struct{ Sector struct{ Code string } }
		return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&body) == nil && body.Sector.Code == "CURRENT_REPLAY"
	})
	p.shutdown()
	p.launch(t, ctx)
	latest, _ := p.awaitEdition(t, wait, "")
	for _, check := range []struct {
		date string
		asOf time.Time
		want []refdata.DatedSector
	}{
		{sectorDay, oldHorizon, original},
		{sectorDay, correctedHorizon, corrected},
		{nextSectorDay, latest, next},
	} {
		got, err := p.source.At(ctx, sectorTenant, check.date, check.asOf, sectorInstruments)
		if err != nil || !reflect.DeepEqual(got, check.want) {
			t.Fatal("restarted Datamaster changed a dated classification edition")
		}
	}
	missing, err := p.source.At(ctx, sectorTenant, "2020-02-02", latest, sectorInstruments)
	if err != nil || len(missing) != 0 {
		t.Fatal("Datamaster substituted a classification for an absent day")
	}
	foreign, err := refdata.NewSectorHistory("http://"+p.historyAddr, "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.At(ctx, "other", sectorDay, latest, sectorInstruments); err == nil || err.Error() != "refdata: Datamaster history HTTP 404" {
		t.Fatal("foreign caller reached the dated classification service")
	}
	for _, test := range []struct{ addr, subject string }{
		{p.historyAddr, "other-workload"},
		{p.liveAddr, "accounting"},
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+test.addr+refdata.SectorHistoryPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		auth.SetPrincipalHeaders(req.Header, test.subject, sectorTenant, nil)
		resp, err := (&http.Client{Timeout: time.Second}).Do(req)
		if err != nil {
			t.Fatal("dated classification listener scope probe failed")
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatal("dated classification route escaped the private Accounting listener")
		}
	}
	for tenant, want := range map[string]int{sectorTenant: 6, "other": 0} {
		pool, err := pg.NewTenantPool(ctx, p.storeURL, tenant)
		if err != nil {
			t.Fatal("open restricted Datamaster read proof")
		}
		var rows int
		var restricted bool
		err = pool.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolbypassrls
			AND NOT has_column_privilege(current_user,'sector_classification_history','known_at','INSERT, UPDATE')
			AND NOT has_table_privilege(current_user,'sector_classification_history','DELETE, TRUNCATE'),
			(SELECT count(*) FROM sector_classification_history)
			FROM pg_roles WHERE rolname=current_user`).Scan(&restricted, &rows)
		pool.Close()
		if err != nil || !restricted || rows != want {
			t.Fatal("Datamaster history was duplicated or visible under foreign tenant RLS")
		}
	}
	t.Logf("Datamaster retained both effective dates and six immutable sector revisions after restart")
}

func classReference(t *testing.T, rows []refdata.DatedSector) string {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("datamaster:%x", sha256.Sum256(raw))
}
