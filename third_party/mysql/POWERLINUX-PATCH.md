# Bounded incoming MySQL packets

This directory is an audited local copy of github.com/go-sql-driver/mysql
v1.10.1, obtained from the Go module cache after standard module checksum
verification. The upstream LICENSE is retained. TLS, authentication and the
wire protocol remain upstream implementations.

PowerLinux adds a guard in readPacket before the driver allocates or reads an
incoming packet body: the accumulated logical packet must fit
Config.MaxAllowedPacket. PowerLinux sets that limit to two MiB for database
profiles. Compression remains disabled for those profiles.

The receive check also applies the smaller configured bound during the login
handshake, before the upstream connection switches from its initial 16 MiB
protocol limit to its configured limit. The upstream configured default remains
64 MiB; PowerLinux profiles explicitly select two MiB. Authentication and TLS
are unchanged.

The regression tests exercise actual wire packets, reject an oversized header
before reading its body, and retain normal bounded packets. Recheck the patch
and those tests when updating the upstream version.

One upstream test is adapted for deterministic cloud execution:
`TestConnectorReturnsTimeout` previously dialed the public address
`1.1.1.1:1234` and expected a timeout, but that address can legitimately refuse
connections. It now calls the same real connector through its documented
`DialFunc`, verifies propagation of the ten millisecond deadline and actual
expiry, and checks the typed network timeout. This test adaptation is separate
from the production receive guard. No upstream test is disabled.
