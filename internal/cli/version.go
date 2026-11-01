package cli

// make build/install replace this with the build time in Asia/Shanghai.
// Direct go build/install retain the source update time as a fallback.
var buildTimestamp = "20261101.170107"

func displayVersion() string { return Version + "+" + buildTimestamp }
