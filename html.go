package main

import (
	"html/template"
	"log"
)

var (
	homeTemplate, moduleTemplate, buildTemplate *template.Template
)

func init() {
	var err error

	buildTemplate, err = template.New("build").Parse(buildTemplateString + htmlTemplateString)
	if err != nil {
		log.Fatalf("parsing build html template: %v", err)
	}

	moduleTemplate, err = template.New("module").Parse(moduleTemplateString + htmlTemplateString)
	if err != nil {
		log.Fatalf("parsing module html template: %v", err)
	}

	homeTemplate, err = template.New("home").Parse(homeTemplateString + htmlTemplateString)
	if err != nil {
		log.Fatalf("parsing home html template: %v", err)
	}
}

const htmlTemplateString = `
{{ define "html" }}
<!doctype html>
<html>
	<head>
		<title>{{ template "title" . }}</title>
		<meta charset="utf-8" />
		<meta name="viewport" content="width=device-width">
		<style>
/*<![CDATA[*/
* { box-sizing: border-box; }
body { margin: 0 auto; max-width: 50rem; font-family: Ubuntu, Lato, sans-serif; color: #111; line-height: 1.3; }
body, input, button { font-size: 17px; }
h1 { font-size: 1.5rem; }
h2 { font-size: 1.25rem; }
h3 { font-size: 1.125rem; }
a { color: #007d9c; }
pre { font-size: .9rem; }
.buildlink { padding: 0 .2rem; display: inline-block; }
.buildlink.unsupported { color: #aaa; }
.buildlink.active { padding: .1rem .2rem; border-radius: .2rem; color: white; background-color: #007d9c; }
.buildlink.unsupported.active { color: white; background-color: #aaa; }
.success { color: #14ae14; }
.failure { color: #d34826; }
.pending { color: #207ef2; }
.output { margin-left: calc(-50vw + 25rem); width: calc(100vw - 2rem); }
.prewrap { white-space: pre-wrap; }
var:before { content: "<" }
var:after { content: ">" }
var { color: #111; font-style: normal; }
.refadein { animation: refadein .5s ease; }
@keyframes refadein {
	0% { opacity: 0; }
	100% { opacity: 1; }
}
/*]]>*/
		</style>
	</head>
	<body>
		<div style="margin:1rem 1rem 3rem 1rem">
{{ template "content" . }}
			<div style="text-align: center; margin-top: 2rem; font-size: .85rem; color: #888"><a style="color:#888" href="https://github.com/mjl-/gobuild">gobuild</a> {{ .GobuildVersion }}</div>
		</div>
{{ template "script" . }}
	</body>
</html>
{{ end }}
{{ template "html" . }}
`

const buildTemplateString = `
{{ define "title" }}{{ .Req.Mod }}@{{ .Req.Version }}{{ .Req.Dir }} - {{ .Req.Goos }}/{{ .Req.Goarch }} {{ .Req.Goversion }}{{ if .Success}} - {{ .Sum }}{{ end }}{{ end }}
{{ define "content" }}
	<p><a href="/">&lt; Home</a></p>
	<h1>
		{{ .Req.Mod }}@{{ .Req.Version }}{{ .Req.Dir }}<br/>
		{{ .Req.Goos }}/{{ .Req.Goarch }} {{ .Req.Goversion }}<br/>
		{{ if .Success}}{{ .Sum }}{{ end }}
		{{ if .Success }}<span class="success">✓</span>{{ else if .InProgress }}<span class="pending">⌛</span>{{ else }}<span class="failure">❌</span>{{ end }}
	</h1>

{{ if .Success }}
	<h2>Download</h2>
	<table>
		<tr>
			<td><a href="{{ .DownloadFilename }}">{{ .DownloadFilename }}</a></td>
			<td style="padding-left: 1rem; text-align: right">{{ .Filesize }}</td>
		</tr>
		<tr>
			<td><a href="{{ .DownloadFilename }}.gz">{{ .DownloadFilename }}.gz</a></td>
			<td style="padding-left: 1rem; text-align: right">{{ .FilesizeGz }}</td>
		</tr>
	</table>
	<p>To download using the transparency log:</p>
	<pre style="margin-left:2rem">gobuild get -sum {{ .Sum }} -target {{ .Req.Goos }}/{{ .Req.Goarch }} -goversion {{ .Req.Goversion }} {{ .Req.Mod }}@{{ .Req.Version }}{{ .Req.Dir }}</pre>

	<h2>More</h2>
	<ul>
		<li><a href="log">Build log</a></li>
		<li><a href="/b/{{ .Req.Mod }}@latest/{{ .DirAppend }}{{ .Req.Goos }}-{{ .Req.Goarch }}-latest/">{{ .Req.Mod }}@<b>latest</b>/{{ .DirAppend }}{{ .Req.Goos }}-{{ .Req.Goarch }}-<b>latest</b>/</a> (<a href="/b/{{ .Req.Mod }}@latest/{{ .DirAppend }}{{ .Req.Goos }}-{{ .Req.Goarch }}-latest/dl">direct download</a>)</li>
		<li>Documentation at <a href="{{ .PkgGoDevURL }}">pkg.go.dev</a></li>
	</ul>
{{ else if .InProgress }}
	<h2>Progress <img style="visibility: hidden; width: 32px; height: 32px;" id="dance" src="/img/gopher-dance-long.gif" title="Dancing gopher, by Ego Elbre, CC0" /></h2>
	<div id="progress">
		<p>Connecting to server to request build and receive updates...</p>
		<p>If your browser has JavaScript disabled, or does not support server-sent events (SSE), follow this <a href="dl">download link</a> to trigger a build.</p>
	</div>

	<h2>Download</h2>
	<p>To download using the transparency log:</p>
	<pre style="margin-left:2rem">gobuild get -target {{ .Req.Goos }}/{{ .Req.Goarch }} -goversion {{ .Req.Goversion }} {{ .Req.Mod }}@{{ .Req.Version }}{{ .Req.Dir }}</pre>
{{ else }}
	<h2>Error</h2>
	<div class="output">
		<pre class="prewrap">
{{ .Output }}
		</pre>
	</div>
{{ end }}

	<h2>Reproduce</h2>
	<p>To reproduce locally:</p>
<pre style="margin-left:2rem">
GO19CONCURRENTCOMPILATION=0 GO111MODULE=on GOPROXY={{ .GoProxy }} \
	CGO_ENABLED=0 GOOS={{ .Req.Goos }} GOARCH={{ .Req.Goarch }} \
	{{ .Req.Goversion }} get -trimpath -ldflags=-buildid= -- {{ .Req.Mod }}{{ .DirPrepend }}@{{ .Req.Version }}
</pre>

	<div style="width: 32%; display: inline-block; vertical-align: top">
		<h2>Module versions</h2>
	{{ if .Mod.Err }}
		<div>error: {{ .Mod.Err }}</div>
	{{ else }}
	{{ range .Mod.VersionLinks }}	<div><a href="{{ .URLPath }}" class="buildlink{{ if .Active }} active{{ end }} ">{{ .Version }}</a>{{ if .Success }}<span class="success">✓</span>{{ end }}</div>{{ end }}
	{{ end }}
	</div>

	<div style="width: 32%; display: inline-block; vertical-align: top">
		<h2>Targets</h2>
	{{ range .TargetLinks }}	<div><a href="{{ .URLPath }}" class="buildlink{{ if .Active }} active{{ end }} ">{{ .Goos }}/{{ .Goarch }}</a>{{ if .Success }}<span class="success">✓</span>{{ end }}</div>{{ end }}
	</div>

	<div style="width: 32%; display: inline-block; vertical-align: top">
		<h2>Go versions</h2>
	{{ range .GoversionLinks }}	<div><a href="{{ .URLPath }}" class="buildlink{{ if .Active }} active{{ end }} {{ if not .Supported }} unsupported{{ end }}">{{ .Goversion }}</a>{{ if .Success }}<span class="success">✓</span>{{ end }}</div>{{ end }}
	</div>
{{ end }}
{{ define "script" }}
	{{ if .InProgress }}
	<script>
(function() {
	function show(e) { e.style.visibility = 'visible' }
	function hide(e) { e.style.visibility = 'hidden' }
	function text(s) { return document.createTextNode(s) }
	function elem(tag) {
		const t = tag.split('.')
		const e = document.createElement(t.shift())
		if (t.length > 0) {
			e.className = t.join(' ')
		}
		for (let i = 1; i < arguments.length; i++) {
			let a = arguments[i]
			if (typeof a === 'string') {
				a = text(a)
			}
			e.appendChild(a)
		}
		return e
	}
	function refadein(e) {
		e.classList.add('refadein')
		setTimeout(function(){ e.classList.remove('refadein') }, 500)
	}

	const progress = document.getElementById('progress')
	function display() {
		while (progress.firstChild) {
			progress.removeChild(progress.firstChild)
		}
		var args = Array.prototype.slice.call(arguments)
		args.unshift('div')
		const e = elem.apply(undefined, args)
		refadein(e)
		progress.appendChild(e)
	}

	if (!window.EventSource) {
		console.log('cannot request build and receive updates, browser does not support server-sent events (SSE)?')
		return
	}

	const dance = document.getElementById('dance')

	var requestBuildWithUpdates = function() {
		const src = new EventSource('events')
		src.addEventListener('update', function(e) {
			const update = JSON.parse(e.data)
			switch (update.Kind) {
			case 'QueuePosition':
				show(dance)
				if (update.QueuePosition === 0) {
					display('Build in progress, hang in there!')
				} else if (update.QueuePosition === 1) {
					display('Waiting in queue, 1 build before yours...')
				} else {
					display('Waiting in queue, ' + update.QueuePosition + ' builds before yours...')
				}
				break;
			case 'TempFailed':
				hide(dance)
				display(
					elem('p', 'Build failed, temporary failure, try again later.'),
					elem('h3', 'Error'),
					elem('pre.prewrap', update.Error),
				)
				src.close()
				break;
			case 'PermFailed':
				{
					hide(dance)
					const link = elem('a', 'build failure output log')
					link.setAttribute('href', 'log')

					display(
						elem('p',
							'Build failed. See ',
							link,
							' for details.',
						),
						elem('h3', 'Error'),
						elem('pre.prewrap', update.Error),
					)
					src.close()
				}
				break;
			case 'Success':
				{
					hide(dance)
					const resultsURL = '/r/' + location.pathname.substring(3) + update.Result.Sum + '/'
					const link = elem('a', 'build results page')
					link.setAttribute('href', resultsURL)

					display(
						elem('p', 'Build successful, redirecting to ', link, '.')
					)
					src.close()
					location.href = resultsURL
				}
				break;
			default:
				console.log('unknown update kind')
			}
		})
		src.addEventListener('open', function(e) {
			show(dance)
			display(elem('p', 'Connected! Waiting for updates...'))
		})
		src.addEventListener('error', function(e) {
			hide(dance)
			if (src) {
				src.close()
			}
			const reconnect = elem('a', 'Reconnect')
			reconnect.setAttribute('href', '#')
			reconnect.addEventListener('click', function(e) {
				e.preventDefault()
				requestBuildWithUpdates()
			})
			display(elem('p', 'Connection to backend failed. ', reconnect, '.'))
		})
	}

	requestBuildWithUpdates()
})()
	</script>
	{{ end }}
{{ end }}
`

const moduleTemplateString = `
{{ define "title" }}{{ .Module }}@{{ .Version }}{{ end }}
{{ define "content" }}
	<p><a href="/">&lt; Home</a></p>
	<h1>{{ .Module }}@{{ .Version }}</h1>
	<p>Main packages:</p>
	<ul>
{{ range .Mains }}		<li><a href="{{ .Link }}">{{ .Name }}</a></li>{{ end }}
	</ul>
{{ end }}
{{ define "script" }}{{ end }}
`

const homeTemplateString = `
{{ define "title" }}Gobuild: Reproducible binaries for the Go module proxy{{ end }}
{{ define "content" }}
		<h1>Gobuild: reproducible binaries with the Go module proxy</h1>
		<p>Gobuild deterministically compiles programs written in Go that are available through the Go module proxy, and returns the binary.</p>

		<p>The <a href="https://proxy.golang.org/">Go module proxy</a> ensures source code stays available, and you are highly likely to get the same code each time you fetch it. Gobuild aims to do the same for binaries.</p>

		<h2>Try a module</h2>
		<form onsubmit="location.href = '/m/' + moduleName.value; return false" method="GET" action="/m/">
			<input onfocus="modulenote.style.display='block'" id="moduleName" name="m" type="text" placeholder="github.com/your/project containing go.mod" style="width:30rem; max-width:75%" />
			<button type="submit">Go!</button>
		</form>
		<p style="display:none" id="modulenote">Note: Point to the module root, the directory that contains the go.mod file, not a package subdirectory. If your module has multiple main commands, they will be listed.</p>

		<h2>Recent builds</h2>
		<div style="white-space: nowrap">
{{ if not .Recents }}<p>No builds yet.</p>{{ end }}
			<ul>
{{ range .Recents }}			<li><a href="{{ . }}">{{ . }}</a></li>{{ end }}
			</ul>
		</div>

		<h2>URLs</h2>
		<p>You can compose URLs to a specific module, build or result:</p>
		<blockquote style="color:#666; white-space: nowrap">
			<div>/m/<var>module</var></div>
			<div>/b/<var>module</var>@<var>version</var>/<var>package</var>/<var>goos</var>-<var>goarch</var>-<var>goversion</var>/</div>
			<div>/r/<var>module</var>@<var>version</var>/<var>package</var>/<var>goos</var>-<var>goarch</var>-<var>goversion</var>/<var>sum</var>/</div>
		</blockquote>

		<h3>Examples</h3>
		<blockquote style="color:#666; white-space: nowrap">
			<a href="/m/github.com/mjl-/gobuild">/m/github.com/mjl-/gobuild</a><br/>
			<a href="/b/github.com/mjl-/sherpa@latest/cmd/sherpaclient/linux-amd64-latest/">/b/github.com/mjl-/sherpa@latest/cmd/sherpaclient/linux-amd64-latest/</a><br/>
			<a href="/r/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.14.1/0m32pSahHbf-fptQdDyWD87GJNXI/">/r/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.14.1/0m32pSahHbf-fptQdDyWD87GJNXI/</a>
		</blockquote>

		<p>The first URL fetches the requested Go module to find the commands (main
packages). In case of a single command, it redirects to a URL of the second
form. In case of multiple commands, it lists them, linking to URLs of the second
form. Links are to the latest module and Go versions, and with goos/goarch
guessed based on user-agent.</p>

		<p>The second URL first resolves "latest" for the module and Go version with a
redirect. For URLs with explicit versions, it starts a build for the requested
parameters if no build is available yet. After a successful build, it redirects
to a URL of the third kind.</p>

		<p>The third URL represents a successful build. The URL includes the sum: The
versioned raw-base64-url-encoded 20-byte prefix of the sha256 sum. The page
links to the binary, the build output log file, and to builds of the same
command with different module versions, goversions, goos/goarch.</p>

		<p>You need not and cannot refresh a successful build: it would yield the same result.</p>

		<h2>Transparency log</h2>

		<p>Gobuild maintains a transparency log containing the hashes of all successful builds, similar to the <a href="https://sum.golang.org/">Go module checksum database</a>. Gobuild's "get" subcommand looks up a content hash through the transparency log, locally keeping track of the last known tree state.  This ensures the list of successful builds and their hashes is append-only, and modifications or removals by the server will be detected when you run "gobuild get".</p>

		<h3>Examples</h3>
		<pre style="margin-left: 2rem">gobuild get github.com/mjl-/gobuild@latest
gobuild get -sum 0N7e6zxGtHCObqNBDA_mXKv7-A9M -target linux/amd64 -goversion go1.14.1 github.com/mjl-/gobuild@v0.0.8</pre>

		{{ if .VerifierKey }}
		<p>If you run your own gobuild instance, specify the verifierkey on the command-line:</p>
		<pre style="margin-left:2rem">gobuild get -verifierkey {{ .VerifierKey }} ...</pre>
		{{ else }}
		<p>NOTE: The transparency log is not configured for this gobuild instance.</p>
		{{ end }}

		<h2>Details</h2>
		<p>Only "go build" is run, for pure Go code. None of "go test", "go generate", build tags, cgo, custom compile/link flags, makefiles, etc. This means gobuild cannot build all Go applications.</p>
		<p>Gobuild looks up module versions through the Go module proxy. That's why shorthand versions like "@v1" don't resolve.</p>
		<p>Gobuild automatically downloads a Go toolchain (SDK) from <a href="https://golang.org/dl/">https://golang.org/dl/</a> when it is first referenced. It also periodically queries that page for the latest supported releases, for redirecting to the latest supported toolchains.</p>
		<p>Gobuild can be configured to verify builds with other gobuild instances, requiring all to return the same hash for a build to be considered successful.</p>
		<p>To build, gobuild executes:</p>
	<pre style="margin-left: 2rem">GO19CONCURRENTCOMPILATION=0 GO111MODULE=on GOPROXY=https://proxy.golang.org/ \
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
  $goversion get -trimpath -ldflags=-buildid= -- $module/$package@$version</pre>
		<p>It's easy to run a local or internal gobuild instance. For configuration details, see this <a href="/emptyconfig">empty example config</a>.</p>
		<p>Code is available at <a href="https://github.com/mjl-/gobuild">github.com/mjl-/gobuild</a>, under MIT-license.</p>

		<h2>Why gobuild</h2>
		<p>Get binaries for any module without having a Go toolchain installed: Useful when working on a machine that's not yours, or for your colleagues or friends who don't have a Go compiler installed.</p>
		<p>Simplify your software release process: You no longer need to cross compile for many architectures and upload binaries to a release page. You never forget a GOOS/GOARCH target. Just link to the build URL for your module and binaries will be created on demand.</p>
		<p>Binaries for the most recent Go toolchain: Go binaries include the runtime and standard library of the Go toolchain used to compile, including bugs. Gobuild links or can redirect to binaries built with the latest Go toolchain, so no need to publish new binaries after an updated Go toolchain is released.</p>
		<p>Verify reproducibility: Configure gobuild to check against other gobuild instances with different configuration to build trust that your binaries are indeed reproducible.</p>
{{ end }}
{{ define "script" }}{{ end }}
`
