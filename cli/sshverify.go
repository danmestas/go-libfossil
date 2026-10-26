package cli

// The commands verify SSH signatures (repo clone --seed-key, a store's
// horizon-seed-key), so they link the verifier the library leaves out.
import _ "github.com/danmestas/go-libfossil/sshverify"
