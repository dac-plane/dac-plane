#!/bin/sh
# Render opencodex config.json from env-injected secrets into the user-writable
# OPENCODEX_HOME, then start the Responses->provider translation proxy.
# Secrets are never logged or baked into the image layer.
set -eu

if [ -z "${CAMER_API_KEY:-}" ]; then
  echo "FATAL: CAMER_API_KEY not set" >&2
  exit 1
fi
if [ -z "${OCX_ADMISSION_TOKEN:-}" ]; then
  echo "FATAL: OCX_ADMISSION_TOKEN not set" >&2
  exit 1
fi

cat > "$OPENCODEX_HOME/config.json" <<EOF
{
  "port": 10100,
  "hostname": "0.0.0.0",
  "defaultProvider": "camer-digital",
  "providers": {
    "camer-digital": {
      "adapter": "openai-chat",
      "baseUrl": "https://api.ai.camer.digital/v1",
      "apiKey": "${CAMER_API_KEY}"
    }
  },
  "combos": {
    "camer-coding": {
      "strategy": "failover",
      "stickyLimit": 1,
      "defaultEffort": null,
      "targets": [
        { "provider": "camer-digital", "model": "glm-5.3-flash", "weight": 1 },
        { "provider": "camer-digital", "model": "deepseek-v4-flash-0731", "weight": 1 }
      ],
      "alias": "camer-digital/glm-5.3-flash"
    }
  },
  "apiKeys": [
    { "id": "occ-agent", "name": "occ-agent", "key": "${OCX_ADMISSION_TOKEN}" }
  ]
}
EOF
chmod 600 "$OPENCODEX_HOME/config.json"

exec opencodex start --port 10100
