package serve

import (
	"embed"
	"fmt"
	"io/fs"
)

// The console ships its own templates and assets embedded in the binary, so the
// running process serves them from itself and never reads the OS filesystem:
// there is no path a request can influence, and no asset can be swapped
// underneath a running server. Both directives live here so the one place a
// reviewer must read to see exactly what ships is the one place the files are
// named.
//
// The templates are embedded as a directory glob so a later page is added by
// adding a file, but the assets are embedded BY NAME: there is deliberately no
// assets/* wildcard, because every asset this binary serves should be one a
// reviewer chose to ship, not every file that happens to sit in the directory.
//
// htmx is vendored as a pinned artefact and its bytes must never be edited:
//
//	htmx 2.0.7 -- https://unpkg.com/htmx.org@2.0.7/dist/htmx.min.js
//	51,076 bytes; SHA-256 60231ae6ba9db3825eb15a261122d5f55921c4d53b66bf637dc18b4ee27c79f9
//	Licensed Zero-Clause BSD (0BSD); the licence text ships as assets/htmx.LICENSE.
//
// htmx is the only script, and it is progressive enhancement: the filter form
// is an ordinary GET form and every link is an ordinary link, so with
// JavaScript off the console still works. Vendoring an asset is not adding a Go
// module, so no go.mod entry and no network fetch at runtime (design section 7).

//go:embed templates/*.html
var templateSources embed.FS

//go:embed assets/app.css assets/htmx.min.js assets/htmx.LICENSE
var assetSources embed.FS

// loadAssets reads every embedded asset into a name-keyed map, so the asset
// handler looks a request up by exact name and can never reach a file the
// binary did not embed. A missing directory or an unreadable entry is a
// construction error, not a per-request surprise.
func loadAssets() (map[string][]byte, error) {
	entries, err := fs.ReadDir(assetSources, "assets")
	if err != nil {
		return nil, fmt.Errorf("serve: read embedded assets: %w", err)
	}
	assets := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := fs.ReadFile(assetSources, "assets/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("serve: read embedded asset %q: %w", entry.Name(), err)
		}
		assets[entry.Name()] = data
	}
	return assets, nil
}
