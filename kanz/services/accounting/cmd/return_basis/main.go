// Command return_basis retains a signed daily close bundle from an enrolled issuer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/eighred/kanz/internal/pg"
	"github.com/eighred/kanz/pkg/bus"
	"github.com/eighred/kanz/services/accounting/internal/return_basis"
)

func boundedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("return basis: input unavailable")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("return basis: bounded regular input required")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("return basis: input unreadable or too large")
	}
	return b, nil
}

func run(ctx context.Context, bodyPath, signaturePath string) (string, error) {
	raw, err := boundedFile(bodyPath, returnbasis.Limit)
	if err != nil {
		return "", err
	}
	sig, err := boundedFile(signaturePath, 64)
	if err != nil {
		return "", err
	}
	dsn, tenant := os.Getenv("ACCOUNTING_DATABASE_URL"), os.Getenv("ACCOUNTING_TENANT")
	if dsn == "" || tenant == "" {
		return "", errors.New("return basis: explicit Accounting database and tenant required")
	}
	pool, err := pg.NewTenantPool(ctx, dsn, tenant)
	if err != nil {
		return "", errors.New("return basis: scoped database unavailable")
	}
	defer pool.Close()
	var limited bool
	err = pool.QueryRow(ctx, `SELECT NOT rolsuper AND NOT rolbypassrls
		AND NOT has_table_privilege(current_user,'return_basis_authorities','INSERT, UPDATE, DELETE, TRUNCATE')
		AND NOT has_table_privilege(current_user,'ledger_entries','INSERT, UPDATE, DELETE, TRUNCATE')
		FROM pg_roles WHERE rolname=current_user`).Scan(&limited)
	if err != nil || !limited {
		return "", errors.New("return basis: restricted source login required")
	}
	receipt, err := returnbasis.NewStore(pool).Accept(bus.WithTenantID(ctx, tenant), raw, sig)
	if err != nil {
		return "", errors.New("return basis: admission failed; verify issuer, scope, horizon and predecessor")
	}
	return receipt.Digest, nil
}

func main() {
	body := flag.String("evidence", "", "Kanz Accounting canonical signed close JSON")
	signature := flag.String("signature", "", "detached Ed25519 signature")
	flag.Parse()
	if *body == "" || *signature == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	digest, err := run(ctx, *body, *signature)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(digest)
}
