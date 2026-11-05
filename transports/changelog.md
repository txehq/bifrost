## 🐞 Fixed

- ElevenLabs speech streams keep their final chunk. The read loop in `core/providers/elevenlabs/elevenlabs.go` dropped any data returned together with EOF, so an upstream response with a `Content-Length` lost its last chunk and the stream still sent done. The loop now forwards that data before it ends the stream (TXE-5677, #92)

## 🍴 Fork changes (txehq/bifrost)

- chore: the gateway code since v2.3.4 is only this fix and its tests. v2.3.5 replaces v2.3.4 as `latest` and `latest-ubi9`
- chore: the Helm chart is 2.1.51, the stable chart; `appVersion` is 2.3.5, so its default image is `v2.3.5`
- note: the CLI and Migration CLI are unchanged: v1.0.1 and v0.1.1 remain their stable releases
- note: this release publishes images and the Helm chart. The image and the OCI chart are private packages, so pulling them needs a token with `read:packages`; the `@maximhq` npm packages and the upstream download host do not serve this fork's builds
