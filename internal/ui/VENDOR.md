# Vendored libraries

The web UI uses three files from two libraries, copied into
`static/vendor/` so nothing is fetched when vigil is built or served.
Each is the unchanged file from the package as published on npm;
`vendor_test.go` fails if one changes without this list.

| File | Package (npm) | File in the package | SHA-256 |
|---|---|---|---|
| `standalone.module.js` | `htm@3.1.1` (htm with preact) | `preact/standalone.module.js` | `72284e8e9079c87817145df1110f74e8a2aa040b2fc384922e18dfcb46fc1fd7` |
| `uPlot.esm.js` | `uplot@1.6.32` | `dist/uPlot.esm.js` | `5dd9b3281aa64b461b42d9945f6adb2649d346502b12281a9ae0d46599a80eba` |
| `uPlot.min.css` | `uplot@1.6.32` | `dist/uPlot.min.css` | `df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04` |

npm's integrity of the packages: `htm@3.1.1`
`sha512-983Vyg8NwUE7JkZ6NmOqpCZ+sh1bKv2iYTlUkzlWmA5JD2acKoxd4KVxbMmxX/85mtfdnDmTFoNKcg5DGAvxNQ==`,
`uplot@1.6.32`
`sha512-KIMVnG68zvu5XXUbC4LQEPnhwOxBuLyW1AHtpm6IKTXImkbLgkMy+jabjLgSLMasNuGGzQm/ep3tOkyTxpiQIw==`.

To update one: download the package tarball from the registry
(`https://registry.npmjs.org/<name>/-/<name>-<version>.tgz`), check it
against the integrity the registry lists for that version
(`https://registry.npmjs.org/<name>/<version>`, `dist.integrity`), copy the
file, and change its row here and in `vendor_test.go`. Read the release
notes first: these run in every admin's browser.
