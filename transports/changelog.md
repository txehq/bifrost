## 🐞 Fixed

- ElevenLabs speech streams keep their final chunk. The read loop in `core/providers/elevenlabs/elevenlabs.go` dropped any data returned together with EOF, so an upstream response with a `Content-Length` lost its last chunk and the stream still sent done. The loop now forwards that data before it ends the stream (TXE-5677, #92)

## 🍴 Fork changes (txehq/bifrost)

- chore: the gateway code since v2.3.4 is only this fix and its tests. v2.3.5 replaces v2.3.4 as `latest` and `latest-ubi9`
- note: the Helm chart, CLI and Migration CLI are unchanged, and their workflows skip: 2.1.50 (`appVersion` 2.3.4), v1.0.1 and v0.1.1 remain their stable releases. The chart's default image stays `v2.3.4` until a chart release moves it
- note: this release publishes images only. The image is a private package, so pulling it needs a token with `read:packages`; the `@maximhq` npm packages and the upstream download host do not serve this fork's builds
