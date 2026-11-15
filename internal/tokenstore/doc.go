// Package tokenstore stores the Glovo sessions of linked accounts
// (docs/design/token-store.md, slice C2-1a of epic #11).
//
// A session is encrypted as one blob with Vault transit, under one derived
// key whose context is "mctl-glovo/session/v1|<pid>|<link_id>", and only the
// ciphertext lands in Postgres. Decryption is bound to a PID, which can only
// be built from a verified mctl-api principal. A decrypted session never
// leaves a Lease, which renders as "[redacted]" everywhere and only builds a
// hosted glovoapi.Client.
//
// Everything fails closed:
//   - the store cannot be built without a KillSwitch, and a control row that
//     could not be read counts as killed;
//   - a killed store refuses before any Vault call;
//   - a read that failed is ErrUnavailable, never ErrNotLinked.
//
// Refreshes use a lease with a compare-and-swap fence (owner decision Q7):
// the lease is taken in one short statement, the Glovo refresh runs with no
// transaction open, and the result is written only if the row still has
// the (link_id, version) pair that was read. The lease only stops two
// holders from calling Glovo at once; correctness rests on the fence. A
// lost fence is ErrSuperseded and its tokens are discarded. A write that
// could not be confirmed pins the session in memory: the pinned client is
// refused, never served, until the write lands or is superseded.
package tokenstore
