package codexadapter

// AppServerVersion is the only native protocol version qualified by this adapter.
const AppServerVersion = "0.165.0"

// Official release archives used by the qualification and image build.
const (
	LinuxAMD64Archive  = "codex-app-server-x86_64-unknown-linux-musl.tar.gz"
	LinuxAMD64SHA256   = "5f6940dae95bc711b699f57c4edba522b7de682e267901c37a03038c3b440fc0"
	LinuxARM64Archive  = "codex-app-server-aarch64-unknown-linux-musl.tar.gz"
	LinuxARM64SHA256   = "431e82c7dfa08c2aa97cdd637d09324557c8de8140279184259eefbe871e4e26"
	DarwinARM64Archive = "codex-app-server-aarch64-apple-darwin.tar.gz"
	DarwinARM64SHA256  = "6d5530758a51aa67ef1e8a843145f0a6fc14aef8ceab13a6f5e9dc84d55b716c"
)
