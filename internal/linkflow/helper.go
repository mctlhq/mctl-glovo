package linkflow

// HelperVersion is the release of the glovo-link helper the /link page tells
// people to install. Tags carry no "v" prefix, so "@latest" does not resolve
// to a release and the page has to name one. release-please rewrites the
// marked line on every release (extra-files in release-please-config.json),
// and TestHelperVersionIsTheReleaseVersion fails if it ever drifts from the
// manifest.
const HelperVersion = "0.7.0" // x-release-please-version

// HelperClientID is the public OAuth client document of the helper (CIMD),
// served by the landing worker. It is the value people pass as -client-id.
const HelperClientID = "https://glovo.mctl.ai/glovo-link/client.json"

// SkillURL is the third-party tool that signs in to Glovo and writes the
// session file the helper uploads (MIT).
const SkillURL = "https://github.com/denya/glovo-skill"

// AccessURL is the mctl-api page where a person records their consent and
// requests access before linking.
const AccessURL = "https://api.mctl.ai/glovo/access"
