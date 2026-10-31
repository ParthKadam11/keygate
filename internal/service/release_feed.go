package service

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/tabloy/keygate/internal/model"
)

// FeedFormat is the wire format requested by an auto-update client.
type FeedFormat string

const (
	FeedFormatSparkle  FeedFormat = "sparkle"
	FeedFormatVelopack FeedFormat = "velopack"
	FeedFormatTauri    FeedFormat = "tauri"
	FeedFormatJSON     FeedFormat = "json" // Keygate-native debug format
)

// IsValidFeedFormat reports whether f is one of the supported formats.
func IsValidFeedFormat(f FeedFormat) bool {
	switch f {
	case FeedFormatSparkle, FeedFormatVelopack, FeedFormatTauri, FeedFormatJSON:
		return true
	}
	return false
}

// FeedInput describes what the feed should contain.
// SignedDownloadURL is per-release: callers (handler) should produce a
// presigned GET URL for each release before passing the slice in. We do that
// outside the feed package so the storage layer is only consulted once per
// request rather than re-computed inside three different format renderers.
type FeedInput struct {
	ProductID   string
	ProductName string
	BaseURL     string // public origin, e.g. https://keygate.app — used for atom links
	Releases    []*FeedRelease

	// MinimumSupportedVersion: optional product-level version floor.
	// The Tauri feed carries it with the message as extra fields; the
	// Sparkle appcast marks updates that reach the floor as critical
	// for apps below it (no message: Sparkle has no field for one).
	// Velopack's feed has no place for it, and the server does not
	// enforce it either.
	MinimumSupportedVersion string
	MinimumSupportedMessage string
}

// FeedRelease is the marshal-ready view of one (release, artifact) pair —
// per platform, since the wire formats for Sparkle/Velopack/Tauri all carry
// per-platform binary metadata. The handler resolves which artifact within
// each release matches the caller's `?platform=` and bundles it here.
type FeedRelease struct {
	Release     *model.Release
	Artifact    *model.ReleaseArtifact
	DownloadURL string
}

// ─── Sparkle (appcast.xml) ───
// Spec: https://sparkle-project.org/documentation/publishing/

// sparkleAppcast is the root element of a Sparkle appcast feed.
type sparkleAppcast struct {
	XMLName      xml.Name       `xml:"rss"`
	Version      string         `xml:"version,attr"`
	XMLNSSparkle string         `xml:"xmlns:sparkle,attr"`
	XMLNSDC      string         `xml:"xmlns:dc,attr"`
	Channel      sparkleChannel `xml:"channel"`
}

type sparkleChannel struct {
	Title       string        `xml:"title"`
	Link        string        `xml:"link,omitempty"`
	Description string        `xml:"description,omitempty"`
	Language    string        `xml:"language,omitempty"`
	Items       []sparkleItem `xml:"item"`
}

type sparkleItem struct {
	Title           string             `xml:"title"`
	PubDate         string             `xml:"pubDate,omitempty"`
	SparkleVersion  string             `xml:"sparkle:version,omitempty"`
	SparkleShortVer string             `xml:"sparkle:shortVersionString,omitempty"`
	Description     sparkleDescription `xml:"description"`
	CriticalUpdate  *sparkleCritical   `xml:"sparkle:criticalUpdate,omitempty"`
	Enclosure       sparkleEnclosure   `xml:"enclosure"`
}

// sparkleCritical marks an update critical for apps older than Version:
// Sparkle compares the running CFBundleVersion with it and, when lower,
// removes the option to skip the update.
type sparkleCritical struct {
	Version string `xml:"sparkle:version,attr"`
}

type sparkleDescription struct {
	XMLName xml.Name `xml:"description"`
	Body    string   `xml:",cdata"`
}

type sparkleEnclosure struct {
	XMLName      xml.Name `xml:"enclosure"`
	URL          string   `xml:"url,attr"`
	Length       int64    `xml:"length,attr"`
	Type         string   `xml:"type,attr"`
	SparkleEDSig string   `xml:"sparkle:edSignature,attr,omitempty"`
}

// sparkleSigPattern: raw base64 (no whitespace, no minisign envelope).
// 88 chars covers a padded 64-byte Ed25519 signature.
var sparkleSigPattern = regexp.MustCompile(`^[A-Za-z0-9+/]{86,88}={0,2}$`)

// RenderSparkle produces an appcast.xml body. Returns the bytes ready to
// write to the response.
//
// Sparkle clients fetch this XML, compare sparkle:version against the
// installed version, and download enclosure.url if newer.
//
// Signature handling: Sparkle's edSignature attribute MUST be raw base64
// of the 64-byte Ed25519 signature. If the model holds a value not matching
// that shape we omit the attribute rather than emit a malformed signature
// that would cause Sparkle to reject the entire item.
func RenderSparkle(in FeedInput) ([]byte, error) {
	feed := sparkleAppcast{
		Version:      "2.0",
		XMLNSSparkle: "http://www.andymatuschak.org/xml-namespaces/sparkle",
		XMLNSDC:      "http://purl.org/dc/elements/1.1/",
		Channel: sparkleChannel{
			Title:       in.ProductName + " Updates",
			Link:        in.BaseURL,
			Description: in.ProductName + " release feed",
			Language:    "en",
		},
	}

	for _, r := range in.Releases {
		if r == nil || r.Release == nil || r.Artifact == nil {
			continue
		}
		rel := r.Release
		a := r.Artifact
		pubDate := ""
		if rel.PublishedAt != nil {
			pubDate = rel.PublishedAt.UTC().Format(time.RFC1123Z)
		}
		sig := ""
		if sparkleSigPattern.MatchString(a.Ed25519Sig) {
			sig = a.Ed25519Sig
		}
		// Only an update that reaches the floor gets an app below it
		// back to a supported version, so only those are critical.
		var critical *sparkleCritical
		if min := in.MinimumSupportedVersion; min != "" && semver.Compare("v"+rel.Version, "v"+min) >= 0 {
			critical = &sparkleCritical{Version: min}
		}
		feed.Channel.Items = append(feed.Channel.Items, sparkleItem{
			Title:           rel.Version,
			PubDate:         pubDate,
			SparkleVersion:  rel.Version,
			SparkleShortVer: rel.Version,
			Description: sparkleDescription{
				Body: xmlText(rel.ReleaseNotes),
			},
			CriticalUpdate: critical,
			Enclosure: sparkleEnclosure{
				URL:          r.DownloadURL,
				Length:       a.FileSize,
				Type:         feedContentType(a.ContentType),
				SparkleEDSig: sig,
			},
		})
	}

	body, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("sparkle: marshal: %w", err)
	}
	// Sparkle clients parse XML strictly; emit the standard prolog.
	return append([]byte(xml.Header), body...), nil
}

// xmlText drops what XML 1.0 cannot carry, even in CDATA: control
// characters other than tab, newline and carriage return (an ANSI colour
// code pasted from a CI changelog, say), and invalid UTF-8. One such
// character would make the whole appcast unparseable, every release in
// it with it. A literal "]]>" needs nothing here: encoding/xml splits it
// across CDATA sections itself.
func xmlText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t', r == '\n', r == '\r',
			r >= 0x20 && r <= 0xD7FF,
			r >= 0xE000 && r <= 0xFFFD,
			r >= 0x10000 && r <= 0x10FFFF:
			return r
		}
		return -1
	}, strings.ToValidUTF8(s, ""))
}

// ─── Velopack (releases.{channel}.json) ───
//
// Velopack's client (SimpleWebSource) is configured with a base URL and
// requests {base}/releases.{channel}.json?arch=&os=&rid=&id=&localVersion=,
// then deserializes a VelopackAssetFeed: {"Assets": [...]} with the
// property names below. It picks the highest Version among Type "Full",
// downloads {base}/{FileName} and compares the file's SHA256 with the
// feed's, as upper case hex, ordinal. FileName must be a plain file
// name: Velopack also names its local cache file after it, so a
// presigned URL there (query string and all) breaks the download. The
// handler answers {base}/{FileName} with a redirect to storage.
// Velopack does not check signatures; the hash and HTTPS carry the trust.

// VelopackAsset is one release in a Velopack feed.
type VelopackAsset struct {
	PackageId     string `json:"PackageId"`
	Version       string `json:"Version"`
	Type          string `json:"Type"`
	FileName      string `json:"FileName"`
	SHA256        string `json:"SHA256"`
	Size          int64  `json:"Size"`
	NotesMarkdown string `json:"NotesMarkdown,omitempty"`
}

// VelopackFeed is the body of releases.{channel}.json.
type VelopackFeed struct {
	Assets []VelopackAsset `json:"Assets"`
}

// BuildVelopack assembles a Velopack feed. packageID is the app's
// Velopack id; Velopack does not filter on it, but it is reported back.
func BuildVelopack(in FeedInput, packageID string) VelopackFeed {
	out := VelopackFeed{Assets: make([]VelopackAsset, 0, len(in.Releases))}
	for _, r := range in.Releases {
		if r == nil || r.Release == nil || r.Artifact == nil {
			continue
		}
		out.Assets = append(out.Assets, VelopackAsset{
			PackageId:     packageID,
			Version:       r.Release.Version,
			Type:          "Full",
			FileName:      VelopackFileName(r.Release, r.Artifact),
			SHA256:        strings.ToUpper(r.Artifact.SHA256),
			Size:          r.Artifact.FileSize,
			NotesMarkdown: r.Release.ReleaseNotes,
		})
	}
	return out
}

// VelopackFileName names a package in a Velopack feed:
// {version}_{channel}_{platform}.nupkg. Semantic versions cannot contain
// an underscore, so the name splits back unambiguously.
func VelopackFileName(rel *model.Release, a *model.ReleaseArtifact) string {
	channel := rel.Channel
	if channel == "" {
		channel = model.ReleaseChannelStable
	}
	return rel.Version + "_" + channel + "_" + a.Platform + ".nupkg"
}

// ParseVelopackFileName reverses VelopackFileName.
func ParseVelopackFileName(name string) (version, channel, platform string, ok bool) {
	parts := strings.Split(strings.TrimSuffix(name, ".nupkg"), "_")
	if len(parts) != 3 || !strings.HasSuffix(name, ".nupkg") || parts[0] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// VelopackPlatform maps a .NET runtime identifier, which Velopack sends
// as rid, to a Keygate platform. "" when it is not one Keygate serves.
func VelopackPlatform(rid string) string {
	switch strings.ToLower(rid) {
	case "win-x64":
		return "windows-x64"
	case "win-arm64":
		return "windows-arm64"
	case "osx-x64":
		return "darwin-x64"
	case "osx-arm64":
		return "darwin-arm64"
	case "linux-x64":
		return "linux-x64"
	case "linux-arm64":
		return "linux-arm64"
	case "linux-arm":
		return "linux-armhf"
	}
	return ""
}

// VelopackChannel maps the channel in releases.{channel}.json to a
// Keygate channel, and to a platform when the name carries one.
// Velopack's default channels are the OS names (win, osx, linux), which
// mean stable here; a package built with --channel beta (or alpha, dev,
// stable) reads that Keygate channel. Velopack's docs name a channel per
// runtime when an app ships several architectures, such as win-x64 or
// win-x64-beta: the runtime picks the platform and a trailing Keygate
// channel name picks the channel.
func VelopackChannel(name string) (channel, platform string) {
	name = strings.ToLower(name)
	if model.IsValidReleaseChannel(name) {
		return name, ""
	}
	channel = model.ReleaseChannelStable
	if i := strings.LastIndex(name, "-"); i > 0 && model.IsValidReleaseChannel(name[i+1:]) {
		channel, name = name[i+1:], name[:i]
	}
	return channel, VelopackPlatform(name)
}

// ─── Tauri ───
// Spec: https://tauri.app/v1/guides/distribution/updater
//
// Tauri's updater fetches a JSON document for ONE release at a time
// (the latest applicable). Schema:
//
//	{
//	  "version": "1.2.3",
//	  "pub_date": "2026-05-10T00:00:00Z",
//	  "url": "https://...",
//	  "signature": "untrusted comment...\nbase64sig",
//	  "notes": "..."
//	}

// TauriManifest is the single-release JSON Tauri's updater consumes.
//
// MinimumSupportedVersion + MinimumSupportedMessage are Keygate
// extensions: clients we ship with the official SDK refuse to run an
// installed build below this version. Tauri itself ignores unknown
// fields, so adding them is forward-compatible.
type TauriManifest struct {
	Version                 string `json:"version"`
	PubDate                 string `json:"pub_date,omitempty"`
	URL                     string `json:"url"`
	Signature               string `json:"signature,omitempty"`
	Notes                   string `json:"notes,omitempty"`
	MinimumSupportedVersion string `json:"minimum_supported_version,omitempty"`
	MinimumSupportedMessage string `json:"minimum_supported_message,omitempty"`
}

// BuildTauri returns the manifest for the FIRST entry in in.Releases (which
// is expected to be the latest after semver sort). Returns a zero value when
// no releases are present — callers should 204/404 in that case.
//
// The Signature field is the artifact's stored minisign signature (see
// TauriSignature), made when the artifact was signed.
func BuildTauri(in FeedInput) TauriManifest {
	for _, r := range in.Releases {
		if r == nil || r.Release == nil || r.Artifact == nil {
			continue
		}
		rel := r.Release
		a := r.Artifact
		m := TauriManifest{
			Version:                 rel.Version,
			URL:                     r.DownloadURL,
			Signature:               a.TauriSignature,
			Notes:                   rel.ReleaseNotes,
			MinimumSupportedVersion: in.MinimumSupportedVersion,
		}
		// The message explains the floor; without one it explains nothing.
		if in.MinimumSupportedVersion != "" {
			m.MinimumSupportedMessage = in.MinimumSupportedMessage
		}
		if rel.PublishedAt != nil {
			m.PubDate = rel.PublishedAt.UTC().Format(time.RFC3339)
		}
		return m
	}
	return TauriManifest{}
}

// ─── Helpers ───

// feedContentType normalises content-type for feeds. Sparkle expects something
// like "application/octet-stream" or "application/x-apple-diskimage". Empty
// strings break some clients.
func feedContentType(ct string) string {
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}
