// Builds the E2B template that runs Codex commands for a task. Bubblewrap is
// required: without it Codex cannot enforce workspace-write, and approvals
// lose their meaning. The adapter starts exec-server after create, because a
// template start command is captured at build time and cannot hold a
// per-sandbox token hash.
import { Template, defaultBuildLogger } from 'e2b'

const version = '0.165.0'
const archive = 'codex-x86_64-unknown-linux-musl'
const checksum = 'be635d3486a5bd060d0fff582ae6d30160280c31344dd85719707fa920e187f0'
const release = `https://github.com/openai/codex/releases/download/rust-v${version}/${archive}.tar.gz`

const template = Template()
  .fromBaseImage()
  .setUser('root')
  .runCmd('apt-get update -qq && apt-get install -y -qq bubblewrap && bwrap --version')
  .runCmd(
    [
      'set -e',
      `curl --proto '=https' --proto-redir '=https' -fL --retry 3 -o /tmp/codex.tar.gz ${release}`,
      `echo '${checksum}  /tmp/codex.tar.gz' | sha256sum -c -`,
      'tar -xzf /tmp/codex.tar.gz -C /usr/local/bin',
      `mv /usr/local/bin/${archive} /usr/local/bin/codex`,
      'chmod 0755 /usr/local/bin/codex',
      'rm /tmp/codex.tar.gz',
      `test "$(codex --version)" = "codex-cli ${version}"`,
      'mkdir -p /workspace',
      'chown user:user /workspace',
    ].join('; '),
  )
  .setUser('user')

const name = process.env.DAC_E2B_TEMPLATE_NAME
if (!name) throw new Error('DAC_E2B_TEMPLATE_NAME must name the template to build')
const built = await Template.build(template, name, { cpuCount: 2, memoryMB: 2048, onBuildLogs: defaultBuildLogger() })
console.log(`template_id=${built.templateId}`)
