package syntheticvenue

import (
	"net"
	"testing"

	"github.com/eighred/kanz/internal/testidentity"
	venuepb "github.com/eighred/kanz/kanz-schemas-go/venue/v1"
	"github.com/eighred/kanz/services/oms/internal/conformancetestkit"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Start binds only a disposable loopback listener. Both peers must hold the
// expected workload SVID under the fixture CA.
func Start(t *testing.T, v *Venue, identity *testidentity.Source) (string, *grpc.Server) {
	t.Helper()
	svid, err := identity.GetX509SVID()
	if err != nil || svid.ID.String() != VenueIdentity {
		t.Fatal("synthetic venue identity required")
	}
	config := tlsconfig.MTLSServerConfig(identity, identity, tlsconfig.AuthorizeID(spiffeid.RequireFromString(OMSIdentity)))
	return conformancetestkit.StartVenue(t, v, "", grpc.Creds(credentials.NewTLS(config)))
}

func Client(t *testing.T, address string, identity *testidentity.Source) venuepb.VenueAdapterServiceClient {
	t.Helper()
	host, _, err := net.SplitHostPort(address)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("synthetic venue client requires literal loopback")
	}
	config := tlsconfig.MTLSClientConfig(identity, identity, tlsconfig.AuthorizeID(spiffeid.RequireFromString(VenueIdentity)))
	conn, err := grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(credentials.NewTLS(config)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return venuepb.NewVenueAdapterServiceClient(conn)
}
