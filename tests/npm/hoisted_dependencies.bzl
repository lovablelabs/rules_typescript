"""pnpm's hidden hoist over tests/npm/pnpm-lock.yaml, as pnpm 12.16.1 wrote
it: `node_modules/.modules.yaml#hoistedDependencies` after `pnpm install
--frozen-lockfile` over a scratch copy of the lockfile and its members'
package.json files, and the workspace members it linked beside them under
`node_modules/.pnpm/node_modules`."""

HOISTED = {
    "undici-types@7.24.6": {
        "undici-types": "private",
    },
    "@babel/core@7.29.0(supports-color@7.2.0)": {
        "@babel/core": "private",
    },
    "@babel/plugin-transform-react-jsx-self@7.27.1(@babel/core@7.29.0(supports-color@7.2.0))": {
        "@babel/plugin-transform-react-jsx-self": "private",
    },
    "@babel/plugin-transform-react-jsx-source@7.27.1(@babel/core@7.29.0(supports-color@7.2.0))": {
        "@babel/plugin-transform-react-jsx-source": "private",
    },
    "@rolldown/pluginutils@1.0.0-rc.3": {
        "@rolldown/pluginutils": "private",
    },
    "@types/babel__core@7.20.5": {
        "@types/babel__core": "private",
    },
    "react-refresh@0.18.0": {
        "react-refresh": "private",
    },
    "@bcoe/v8-coverage@1.0.2": {
        "@bcoe/v8-coverage": "private",
    },
    "@vitest/utils@4.1.12": {
        "@vitest/utils": "private",
    },
    "ast-v8-to-istanbul@1.0.5": {
        "ast-v8-to-istanbul": "private",
    },
    "istanbul-lib-coverage@3.2.2": {
        "istanbul-lib-coverage": "private",
    },
    "istanbul-lib-report@3.0.1": {
        "istanbul-lib-report": "private",
    },
    "istanbul-reports@3.2.0": {
        "istanbul-reports": "private",
    },
    "magicast@0.5.4": {
        "magicast": "private",
    },
    "obug@2.1.4": {
        "obug": "private",
    },
    "std-env@4.2.0": {
        "std-env": "private",
    },
    "tinyrainbow@3.1.1": {
        "tinyrainbow": "private",
    },
    "@types/ws@8.5.14": {
        "@types/ws": "private",
    },
    "@types/whatwg-mimetype@3.0.2": {
        "@types/whatwg-mimetype": "private",
    },
    "buffer-image-size@0.6.4": {
        "buffer-image-size": "private",
    },
    "entities@7.0.1": {
        "entities": "private",
    },
    "whatwg-mimetype@3.0.0": {
        "whatwg-mimetype": "private",
    },
    "ws@8.22.1": {
        "ws": "private",
    },
    "@oxlint/binding-linux-x64-gnu@1.90.0": {
        "@oxlint/binding-linux-x64-gnu": "private",
    },
    "@istanbuljs/schema@0.1.3": {
        "@istanbuljs/schema": "private",
    },
    "glob@10.5.0": {
        "glob": "private",
    },
    "minimatch@10.2.4": {
        "minimatch": "private",
    },
    "bundle-require@5.1.0(esbuild@0.28.2)": {
        "bundle-require": "private",
    },
    "cac@7.1.0": {
        "cac": "private",
    },
    "chokidar@5.0.0": {
        "chokidar": "private",
    },
    "consola@3.4.2": {
        "consola": "private",
    },
    "esbuild@0.28.2": {
        "esbuild": "private",
    },
    "fix-dts-default-cjs-exports@1.0.1": {
        "fix-dts-default-cjs-exports": "private",
    },
    "joycon@3.1.1": {
        "joycon": "private",
    },
    "picocolors@1.1.1": {
        "picocolors": "private",
    },
    "postcss-load-config@6.0.1(postcss@8.5.32)": {
        "postcss-load-config": "private",
    },
    "resolve-from@5.0.0": {
        "resolve-from": "private",
    },
    "rollup@4.67.3": {
        "rollup": "private",
    },
    "source-map-js@1.2.2": {
        "source-map-js": "private",
    },
    "sucrase@3.35.1": {
        "sucrase": "private",
    },
    "tinyexec@1.3.1": {
        "tinyexec": "private",
    },
    "tinyglobby@0.2.17": {
        "tinyglobby": "private",
    },
    "tree-kill@1.2.2": {
        "tree-kill": "private",
    },
    "postcss@8.5.32": {
        "postcss": "private",
    },
    "lightningcss@1.33.0": {
        "lightningcss": "private",
    },
    "picomatch@4.0.7": {
        "picomatch": "private",
    },
    "rolldown@1.2.16": {
        "rolldown": "private",
    },
    "@vitest/expect@4.1.12": {
        "@vitest/expect": "private",
    },
    "@vitest/mocker@4.1.12(vite@8.3.5(@types/node@24.19.2)(esbuild@0.28.2))": {
        "@vitest/mocker": "private",
    },
    "@vitest/pretty-format@4.1.12": {
        "@vitest/pretty-format": "private",
    },
    "@vitest/runner@4.1.12": {
        "@vitest/runner": "private",
    },
    "@vitest/snapshot@4.1.12": {
        "@vitest/snapshot": "private",
    },
    "@vitest/spy@4.1.12": {
        "@vitest/spy": "private",
    },
    "es-module-lexer@2.3.2": {
        "es-module-lexer": "private",
    },
    "expect-type@1.3.0": {
        "expect-type": "private",
    },
    "magic-string@0.30.21": {
        "magic-string": "private",
    },
    "pathe@2.0.3": {
        "pathe": "private",
    },
    "tinybench@2.9.0": {
        "tinybench": "private",
    },
    "why-is-node-running@2.3.0": {
        "why-is-node-running": "private",
    },
    "@babel/code-frame@7.29.0": {
        "@babel/code-frame": "private",
    },
    "@babel/generator@7.29.1": {
        "@babel/generator": "private",
    },
    "@babel/helper-compilation-targets@7.28.6": {
        "@babel/helper-compilation-targets": "private",
    },
    "@babel/helper-module-transforms@7.28.6(@babel/core@7.29.0(supports-color@7.2.0))(supports-color@7.2.0)": {
        "@babel/helper-module-transforms": "private",
    },
    "@babel/helpers@7.28.6": {
        "@babel/helpers": "private",
    },
    "@babel/parser@7.29.8": {
        "@babel/parser": "private",
    },
    "@babel/template@7.28.6": {
        "@babel/template": "private",
    },
    "@babel/traverse@7.29.0(supports-color@7.2.0)": {
        "@babel/traverse": "private",
    },
    "@babel/types@7.29.8": {
        "@babel/types": "private",
    },
    "@jridgewell/remapping@2.3.5": {
        "@jridgewell/remapping": "private",
    },
    "convert-source-map@2.0.0": {
        "convert-source-map": "private",
    },
    "debug@4.4.3(supports-color@7.2.0)": {
        "debug": "private",
    },
    "gensync@1.0.0-beta.2": {
        "gensync": "private",
    },
    "json5@2.2.3": {
        "json5": "private",
    },
    "semver@6.3.1": {
        "semver": "private",
    },
    "@babel/helper-plugin-utils@7.28.6": {
        "@babel/helper-plugin-utils": "private",
    },
    "@types/babel__generator@7.27.0": {
        "@types/babel__generator": "private",
    },
    "@types/babel__template@7.4.4": {
        "@types/babel__template": "private",
    },
    "@types/babel__traverse@7.28.0": {
        "@types/babel__traverse": "private",
    },
    "@standard-schema/spec@1.1.0": {
        "@standard-schema/spec": "private",
    },
    "@types/chai@5.2.3": {
        "@types/chai": "private",
    },
    "chai@6.2.2": {
        "chai": "private",
    },
    "estree-walker@3.0.3": {
        "estree-walker": "private",
    },
    "@jridgewell/trace-mapping@0.3.31": {
        "@jridgewell/trace-mapping": "private",
    },
    "js-tokens@10.0.0": {
        "js-tokens": "private",
    },
    "load-tsconfig@0.2.5": {
        "load-tsconfig": "private",
    },
    "readdirp@5.1.1": {
        "readdirp": "private",
    },
    "@esbuild/linux-x64@0.28.2": {
        "@esbuild/linux-x64": "private",
    },
    "mlly@1.8.2": {
        "mlly": "private",
    },
    "foreground-child@3.3.1": {
        "foreground-child": "private",
    },
    "jackspeak@3.4.3": {
        "jackspeak": "private",
    },
    "minipass@7.1.3": {
        "minipass": "private",
    },
    "package-json-from-dist@1.0.1": {
        "package-json-from-dist": "private",
    },
    "path-scurry@1.11.1": {
        "path-scurry": "private",
    },
    "make-dir@4.0.0": {
        "make-dir": "private",
    },
    "supports-color@7.2.0": {
        "supports-color": "private",
    },
    "html-escaper@2.0.2": {
        "html-escaper": "private",
    },
    "detect-libc@2.1.2": {
        "detect-libc": "private",
    },
    "lightningcss-linux-x64-gnu@1.33.0": {
        "lightningcss-linux-x64-gnu": "private",
    },
    "@jridgewell/sourcemap-codec@1.5.5": {
        "@jridgewell/sourcemap-codec": "private",
    },
    "brace-expansion@5.0.4": {
        "brace-expansion": "private",
    },
    "lilconfig@3.1.3": {
        "lilconfig": "private",
    },
    "nanoid@3.3.20": {
        "nanoid": "private",
    },
    "@oxc-project/types@0.156.0": {
        "@oxc-project/types": "private",
    },
    "@rolldown/binding-linux-x64-gnu@1.2.16": {
        "@rolldown/binding-linux-x64-gnu": "private",
    },
    "@types/estree@1.0.10": {
        "@types/estree": "private",
    },
    "@napi-rs/lzma-linux-x64-gnu@1.5.1": {
        "@napi-rs/lzma-linux-x64-gnu": "private",
    },
    "@rollup/rollup-linux-x64-gnu@4.67.3": {
        "@rollup/rollup-linux-x64-gnu": "private",
    },
    "@jridgewell/gen-mapping@0.3.13": {
        "@jridgewell/gen-mapping": "private",
    },
    "commander@4.1.1": {
        "commander": "private",
    },
    "lines-and-columns@1.2.4": {
        "lines-and-columns": "private",
    },
    "mz@2.7.0": {
        "mz": "private",
    },
    "pirates@4.0.7": {
        "pirates": "private",
    },
    "ts-interface-checker@0.1.13": {
        "ts-interface-checker": "private",
    },
    "fdir@6.5.0(picomatch@4.0.7)": {
        "fdir": "private",
    },
    "siginfo@2.0.0": {
        "siginfo": "private",
    },
    "stackback@0.0.2": {
        "stackback": "private",
    },
    "@babel/helper-validator-identifier@7.29.7": {
        "@babel/helper-validator-identifier": "private",
    },
    "jsesc@3.1.0": {
        "jsesc": "private",
    },
    "@babel/compat-data@7.29.0": {
        "@babel/compat-data": "private",
    },
    "@babel/helper-validator-option@7.27.1": {
        "@babel/helper-validator-option": "private",
    },
    "browserslist@4.28.1": {
        "browserslist": "private",
    },
    "lru-cache@5.1.1": {
        "lru-cache": "private",
    },
    "@babel/helper-module-imports@7.28.6(supports-color@7.2.0)": {
        "@babel/helper-module-imports": "private",
    },
    "@babel/helper-globals@7.28.0": {
        "@babel/helper-globals": "private",
    },
    "@babel/helper-string-parser@7.29.7": {
        "@babel/helper-string-parser": "private",
    },
    "@types/deep-eql@4.0.2": {
        "@types/deep-eql": "private",
    },
    "assertion-error@2.0.1": {
        "assertion-error": "private",
    },
    "balanced-match@4.0.4": {
        "balanced-match": "private",
    },
    "ms@2.1.3": {
        "ms": "private",
    },
    "cross-spawn@7.0.6": {
        "cross-spawn": "private",
    },
    "signal-exit@4.1.0": {
        "signal-exit": "private",
    },
    "@isaacs/cliui@8.0.2": {
        "@isaacs/cliui": "private",
    },
    "@pkgjs/parseargs@0.11.0": {
        "@pkgjs/parseargs": "private",
    },
    "acorn@8.18.0": {
        "acorn": "private",
    },
    "pkg-types@1.3.1": {
        "pkg-types": "private",
    },
    "ufo@1.6.4": {
        "ufo": "private",
    },
    "any-promise@1.3.0": {
        "any-promise": "private",
    },
    "object-assign@4.1.1": {
        "object-assign": "private",
    },
    "thenify-all@1.6.0": {
        "thenify-all": "private",
    },
    "string-width@5.1.2": {
        "string-width": "private",
    },
    "string-width@4.2.3": {
        "string-width-cjs": "private",
    },
    "strip-ansi@7.2.0": {
        "strip-ansi": "private",
    },
    "strip-ansi@6.0.1": {
        "strip-ansi-cjs": "private",
    },
    "wrap-ansi@8.1.0": {
        "wrap-ansi": "private",
    },
    "wrap-ansi@7.0.0": {
        "wrap-ansi-cjs": "private",
    },
    "@jridgewell/resolve-uri@3.1.2": {
        "@jridgewell/resolve-uri": "private",
    },
    "baseline-browser-mapping@2.10.0": {
        "baseline-browser-mapping": "private",
    },
    "caniuse-lite@1.0.30001777": {
        "caniuse-lite": "private",
    },
    "electron-to-chromium@1.5.307": {
        "electron-to-chromium": "private",
    },
    "node-releases@2.0.36": {
        "node-releases": "private",
    },
    "update-browserslist-db@1.2.3(browserslist@4.28.1)": {
        "update-browserslist-db": "private",
    },
    "path-key@3.1.1": {
        "path-key": "private",
    },
    "shebang-command@2.0.0": {
        "shebang-command": "private",
    },
    "which@2.0.2": {
        "which": "private",
    },
    "yallist@3.1.1": {
        "yallist": "private",
    },
    "confbox@0.1.8": {
        "confbox": "private",
    },
    "has-flag@4.0.0": {
        "has-flag": "private",
    },
    "thenify@3.3.1": {
        "thenify": "private",
    },
    "shebang-regex@3.0.0": {
        "shebang-regex": "private",
    },
    "emoji-regex@8.0.0": {
        "emoji-regex": "private",
    },
    "is-fullwidth-code-point@3.0.0": {
        "is-fullwidth-code-point": "private",
    },
    "eastasianwidth@0.2.0": {
        "eastasianwidth": "private",
    },
    "ansi-regex@5.0.1": {
        "ansi-regex": "private",
    },
    "escalade@3.2.0": {
        "escalade": "private",
    },
    "isexe@2.0.0": {
        "isexe": "private",
    },
    "ansi-styles@4.3.0": {
        "ansi-styles": "private",
    },
    "color-convert@2.0.1": {
        "color-convert": "private",
    },
    "color-name@1.1.4": {
        "color-name": "private",
    },
}

HOISTED_MEMBERS = ["hoisted-member", "nested-consumer", "nested-shared"]
