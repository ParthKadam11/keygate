package service

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/tabloy/keygate/internal/model"
)

// validBase64Sig generates a syntactically valid 64-byte Ed25519 signature in
// raw base64 form for tests that need to verify Sparkle accepts it.
func validBase64Sig() string {
	raw := make([]byte, 64)
	for i := range raw {
		raw[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func mkRelease(version string, opts ...func(*model.Release, *model.ReleaseArtifact)) *FeedRelease {
	t := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	r := &model.Release{
		ID:           "rel-" + version,
		ProductID:    "prod-1",
		Version:      version,
		Channel:      model.ReleaseChannelStable,
		Name:         "MyApp",
		ReleaseNotes: "Bug fixes",
		Status:       model.ReleaseStatusPublished,
		PublishedAt:  &t,
	}
	a := &model.ReleaseArtifact{
		ID:          "art-" + version,
		ReleaseID:   r.ID,
		Platform:    "darwin-arm64",
		FileKey:     "releases/myapp/" + version + "/darwin-arm64.dmg",
		FileSize:    1024 * 1024,
		SHA256:      "abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
		ContentType: "application/x-apple-diskimage",
	}
	for _, opt := range opts {
		opt(r, a)
	}
	return &FeedRelease{
		Release:     r,
		Artifact:    a,
		DownloadURL: "https://signed.example.com/" + version,
	}
}

func TestRenderSparkleHappyPath(t *testing.T) {
	in := FeedInput{
		ProductID:   "prod-1",
		ProductName: "MyApp",
		BaseURL:     "https://example.com",
		Releases: []*FeedRelease{
			mkRelease("1.2.3", func(_ *model.Release, a *model.ReleaseArtifact) { a.Ed25519Sig = validBase64Sig() }),
		},
	}
	body, err := RenderSparkle(in)
	if err != nil {
		t.Fatalf("RenderSparkle: %v", err)
	}
	s := string(body)
	if !strings.HasPrefix(s, xml.Header) {
		t.Errorf("missing XML prolog")
	}
	for _, want := range []string{
		"<rss version=\"2.0\"",
		"xmlns:sparkle=",
		"<title>MyApp Updates</title>",
		"<sparkle:version>1.2.3</sparkle:version>",
		`url="https://signed.example.com/1.2.3"`,
		`length="1048576"`,
		"sparkle:edSignature=",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("sparkle output missing %q\n--- output ---\n%s", want, s)
		}
	}

	// Must round-trip parse.
	var roundTrip sparkleAppcast
	if err := xml.Unmarshal(body, &roundTrip); err != nil {
		t.Errorf("sparkle output failed to re-parse: %v", err)
	}
	if len(roundTrip.Channel.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(roundTrip.Channel.Items))
	}
}

// The product's minimum supported version marks the updates that reach
// it as critical for apps below it, in the element Sparkle reads:
// <sparkle:criticalUpdate sparkle:version="..."/>.
func TestRenderSparkleCriticalUpdate(t *testing.T) {
	in := FeedInput{
		ProductName:             "MyApp",
		MinimumSupportedVersion: "1.2.0",
		Releases:                []*FeedRelease{mkRelease("1.3.0"), mkRelease("1.2.0"), mkRelease("1.1.5")},
	}
	body, err := RenderSparkle(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `<sparkle:criticalUpdate sparkle:version="1.2.0"></sparkle:criticalUpdate>`) {
		t.Errorf("missing the criticalUpdate element:\n%s", body)
	}
	var feed struct {
		Items []struct {
			Version  string `xml:"version"`
			Critical *struct {
				Version string `xml:"version,attr"`
			} `xml:"criticalUpdate"`
		} `xml:"channel>item"`
	}
	if err := xml.Unmarshal(body, &feed); err != nil {
		t.Fatal(err)
	}
	for _, it := range feed.Items {
		wantCritical := it.Version != "1.1.5" // below the floor: installing it fixes nothing
		if (it.Critical != nil) != wantCritical {
			t.Errorf("%s critical = %v, want %v", it.Version, it.Critical != nil, wantCritical)
		}
		if it.Critical != nil && it.Critical.Version != "1.2.0" {
			t.Errorf("%s critical for apps below %q, want 1.2.0", it.Version, it.Critical.Version)
		}
	}

	in.MinimumSupportedVersion = ""
	body, _ = RenderSparkle(in)
	if strings.Contains(string(body), "criticalUpdate") {
		t.Errorf("no floor must mean no critical updates:\n%s", body)
	}
}

func TestRenderSparkleOmitsInvalidSignature(t *testing.T) {
	in := FeedInput{
		ProductName: "MyApp",
		Releases: []*FeedRelease{
			mkRelease("1.2.3", func(_ *model.Release, a *model.ReleaseArtifact) {
				a.Ed25519Sig = "untrusted comment: garbage\nBASE64_NOT_RIGHT_FORMAT"
			}),
		},
	}
	body, err := RenderSparkle(in)
	if err != nil {
		t.Fatalf("RenderSparkle: %v", err)
	}
	if strings.Contains(string(body), "sparkle:edSignature=") {
		t.Errorf("expected invalid sig to be omitted; got:\n%s", string(body))
	}
}

// Release notes reach Sparkle as written: a "]]>" in them stays inside
// the description (split once, by encoding/xml, not twice), and
// characters XML cannot carry are dropped instead of making the whole
// appcast unparseable.
func TestRenderSparkleNotes(t *testing.T) {
	for _, tc := range []struct{ notes, want string }{
		{"before ]]> middle ]]> after", "before ]]> middle ]]> after"},
		{"fixes: a[i]]> b", "fixes: a[i]]> b"},
		{"\x1b[32mAdded\x1b[0m dark mode", "[32mAdded[0m dark mode"},
		{"tab\there\nnew line\x00\x07 bell", "tab\there\nnew line bell"},
		{"bad utf-8 \xff\xfe here", "bad utf-8  here"},
		{"中文说明 ✓", "中文说明 ✓"},
	} {
		in := FeedInput{
			ProductName: "X",
			Releases: []*FeedRelease{
				mkRelease("1.0.0", func(r *model.Release, _ *model.ReleaseArtifact) { r.ReleaseNotes = tc.notes }),
			},
		}
		body, err := RenderSparkle(in)
		if err != nil {
			t.Fatalf("%q: RenderSparkle: %v", tc.notes, err)
		}
		var rt struct {
			Notes []string `xml:"channel>item>description"`
		}
		if err := xml.Unmarshal(body, &rt); err != nil {
			t.Fatalf("%q: sparkle XML parse failed: %v\n%s", tc.notes, err, body)
		}
		if len(rt.Notes) != 1 || rt.Notes[0] != tc.want {
			t.Errorf("%q: the description reads %q, want %q", tc.notes, rt.Notes, tc.want)
		}
	}
}

// The feed must match what Velopack's client deserializes
// (VelopackAssetFeed): an object with Assets, PascalCase properties, an
// plain FileName it downloads from the base URL, and SHA256 in upper case hex,
// since the client compares its own BitConverter hash ordinally.
func TestBuildVelopack(t *testing.T) {
	in := FeedInput{Releases: []*FeedRelease{mkRelease("1.2.3"), mkRelease("1.3.0")}}
	in.Releases[0].Artifact.SHA256 = "ab12cd34"
	feed := BuildVelopack(in, "MyApp")
	body, err := json.Marshal(feed)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Assets []map[string]any
	}
	if err := json.Unmarshal(body, &decoded); err != nil || len(decoded.Assets) != 2 {
		t.Fatalf("feed must be {\"Assets\": [...]} with 2 entries, got %s", body)
	}
	first := decoded.Assets[0]
	for _, k := range []string{"PackageId", "Version", "Type", "FileName", "SHA256", "Size"} {
		if _, ok := first[k]; !ok {
			t.Errorf("asset is missing %s: %s", k, body)
		}
	}
	if first["PackageId"] != "MyApp" || first["Version"] != "1.2.3" || first["Type"] != "Full" {
		t.Errorf("unexpected asset: %v", first)
	}
	if first["SHA256"] != "AB12CD34" {
		t.Errorf("SHA256 must be upper case hex, got %v", first["SHA256"])
	}
	// A plain file name: Velopack names its cache file after it.
	name := first["FileName"].(string)
	if strings.ContainsAny(name, "/?") {
		t.Errorf("FileName must be a plain file name, got %v", name)
	}
	v, ch, plat, ok := ParseVelopackFileName(name)
	if !ok || v != "1.2.3" || ch == "" || plat == "" {
		t.Errorf("FileName %q must parse back to version, channel and platform", name)
	}
}

func TestBuildVelopackEmpty(t *testing.T) {
	body, err := json.Marshal(BuildVelopack(FeedInput{}, "MyApp"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"Assets":[]}` {
		t.Errorf("an empty feed must be {\"Assets\":[]}, got %s", body)
	}
}

func TestVelopackFileNameRoundTrip(t *testing.T) {
	rel := &model.Release{Version: "2.0.0-beta.1", Channel: "beta"}
	a := &model.ReleaseArtifact{Platform: "darwin-arm64"}
	name := VelopackFileName(rel, a)
	if name != "2.0.0-beta.1_beta_darwin-arm64.nupkg" {
		t.Fatalf("got %q", name)
	}
	v, ch, plat, ok := ParseVelopackFileName(name)
	if !ok || v != rel.Version || ch != "beta" || plat != "darwin-arm64" {
		t.Errorf("round trip gave %q %q %q %v", v, ch, plat, ok)
	}
	if _, _, _, ok := ParseVelopackFileName("releases.osx.json"); ok {
		t.Error("a feed name must not parse as a package")
	}
}

func TestVelopackPlatformAndChannel(t *testing.T) {
	for rid, want := range map[string]string{
		"win-x64": "windows-x64", "win-arm64": "windows-arm64", "osx-arm64": "darwin-arm64",
		"osx-x64": "darwin-x64", "linux-x64": "linux-x64", "linux-arm64": "linux-arm64", "linux-arm": "linux-armhf",
		"freebsd-x64": "",
	} {
		if got := VelopackPlatform(rid); got != want {
			t.Errorf("rid %s: got %q, want %q", rid, got, want)
		}
	}
	for name, want := range map[string][2]string{
		"win": {"stable", ""}, "osx": {"stable", ""}, "linux": {"stable", ""},
		"beta": {"beta", ""}, "stable": {"stable", ""}, "dev": {"dev", ""},
		"win-x64": {"stable", "windows-x64"}, "win-x64-beta": {"beta", "windows-x64"},
		"osx-arm64-alpha": {"alpha", "darwin-arm64"}, "linux-arm": {"stable", "linux-armhf"},
		"nightly": {"stable", ""},
	} {
		if ch, plat := VelopackChannel(name); ch != want[0] || plat != want[1] {
			t.Errorf("channel %s: got %q %q, want %q %q", name, ch, plat, want[0], want[1])
		}
	}
}

func TestBuildTauri(t *testing.T) {
	in := FeedInput{
		ProductName: "MyApp",
		Releases: []*FeedRelease{
			mkRelease("1.2.3"),
			mkRelease("1.3.0"), // Tauri picks first only
		},
	}
	m := BuildTauri(in)
	if m.Version != "1.2.3" {
		t.Errorf("Tauri picked wrong version: %q", m.Version)
	}
	if m.URL == "" {
		t.Errorf("Tauri URL empty")
	}
	if m.PubDate == "" {
		t.Errorf("Tauri pub_date empty")
	}
}

// The message travels only with the floor it explains.
func TestBuildTauriMinimumMessage(t *testing.T) {
	in := FeedInput{Releases: []*FeedRelease{mkRelease("1.3.0")}, MinimumSupportedVersion: "1.2.0", MinimumSupportedMessage: "Please update"}
	if m := BuildTauri(in); m.MinimumSupportedVersion != "1.2.0" || m.MinimumSupportedMessage != "Please update" {
		t.Errorf("floor and message: got %q %q", m.MinimumSupportedVersion, m.MinimumSupportedMessage)
	}
	in.MinimumSupportedVersion = ""
	if m := BuildTauri(in); m.MinimumSupportedMessage != "" {
		t.Errorf("a message without a floor must not be sent, got %q", m.MinimumSupportedMessage)
	}
}

func TestBuildTauriEmpty(t *testing.T) {
	m := BuildTauri(FeedInput{})
	if m.Version != "" {
		t.Errorf("expected empty manifest, got %+v", m)
	}
}

func TestIsValidFeedFormat(t *testing.T) {
	for _, ok := range []FeedFormat{FeedFormatSparkle, FeedFormatVelopack, FeedFormatTauri, FeedFormatJSON} {
		if !IsValidFeedFormat(ok) {
			t.Errorf("expected %q to be valid", ok)
		}
	}
	for _, bad := range []FeedFormat{"", "atom", "rss", "SPARKLE"} {
		if IsValidFeedFormat(bad) {
			t.Errorf("expected %q to be invalid", bad)
		}
	}
}
