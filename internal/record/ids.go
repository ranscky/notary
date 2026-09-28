package record

// RecordID is the stable identifier of a record in Notary's ledger.
type RecordID string

// Hash is a 32-byte content digest used to link records into a tamper-evident
// sequence. It is sized for a SHA-256 output.
type Hash [32]byte

// IdemKey is an idempotency key used to deduplicate record writes.
type IdemKey string
