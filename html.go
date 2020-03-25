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
* { box-sizing: border-box; }
body { margin: 0 auto; max-width: 50rem; font-family: Ubuntu, Lato, sans-serif; color: #111; line-height: 1.3; }
body, input, button { font-size: 17px; }
h1 { font-size: 1.5rem; }
h2 { font-size: 1.25rem; }
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
		</style>
	</head>
	<body>
		<div style="margin:1rem 1rem 3rem 1rem">
{{ template "content" . }}
		</div>
{{ template "script" . }}
	</body>
</html>
{{ end }}
{{ template "html" . }}
`

const buildTemplateString = `
{{ define "title" }}{{ .Req.Mod }}@{{ .Req.Version }}/{{ .Req.Dir }} - {{ .Req.Goos }}/{{ .Req.Goarch }} {{ .Req.Goversion }}{{ if .Success}} - {{ .Sum }}{{ end }}{{ end }}
{{ define "content" }}
	<p><a href="/">&lt; Home</a></p>
	<h1>
		{{ .Req.Mod }}@{{ .Req.Version }}/{{ .Req.Dir }}<br/>
		{{ .Req.Goos }}/{{ .Req.Goarch }} {{ .Req.Goversion }}
		{{ if .Success}}<br/>{{ .Sum }}{{ end }}
		{{ if .Success }}<span class="success">✓</span>{{ else if .InProgress }}<span class="pending">⌛</span>{{ else }}<span class="failure">❌</span>{{ end }}
	</h1>

{{ if .Success }}
	<h2>Download</h2>
	<table>
		<tr>
			<td><a href="{{ .DownloadFilename }}">{{ .DownloadFilename }}</a></td>
			<td style="padding-left: 1rem">{{ .Filesize }}</td>
		</tr>
		<tr>
			<td><a href="{{ .DownloadFilename }}.gz">{{ .DownloadFilename }}.gz</a></td>
			<td style="padding-left: 1rem">{{ .FilesizeGz }}</td>
		</tr>
	</table>

	<h2>More</h2>
	<ul>
		<li><a href="log">Build log</a></li>
		<li><a href="/b/{{ .Req.Mod }}@latest/{{ .Req.Dir }}{{ .Req.Goos }}-{{ .Req.Goarch }}-latest/">{{ .Req.Mod }}@<b>latest</b>/{{ .Req.Dir }}{{ .Req.Goos }}-{{ .Req.Goarch }}-<b>latest</b>/</a> (<a href="/b/{{ .Req.Mod }}@latest/{{ .Req.Dir }}{{ .Req.Goos }}-{{ .Req.Goarch }}-latest/dl">direct download</a>)</li>
		<li>Built on {{ .Start }}, in {{ .BuildWallTimeMS }}ms; sys {{ .SystemTimeMS }}ms, user {{ .UserTimeMS }}ms.</li>
		<li>Documentation at <a href="{{ .PkgGoDevURL }}">pkg.go.dev</a></li>
	</ul>
{{ else if .InProgress }}
	<h2>Progress <img style="visibility: hidden; width: 32px; height: 32px;" id="dance" src="/img/gopher-dance-long.gif" title="Dancing gopher, by Ego Elbre, CC0" /></h2>
	<div id="progress">
		<p>Connecting to server to request build and receive updates...</p>
		<p>If your browser has JavaScript disabled, or does not support server-sent events (SSE), follor this <a href="dl">download link</a> to trigger a build.</p>
	</div>
{{ else }}
	<h2>Error</h2>
	<div class="output">
		<pre class="prewrap">
{{ .Output }}
		</pre>
	</div>
{{ end }}

	<h2>Reproduce</h2>
	<p>You should be able to reproduce this build with the commands below.</p>
<pre>tmpdir=$(mktemp -d)
cd $tmpdir
GO111MODULE=on GOPROXY={{ .GoProxy }} {{ .Req.Goversion }} get -d -v {{ .req.Mod }}@{{ .Req.Version }}
cd $HOME/go/pkg/mod/{{ .Req.Mod }}@{{ .Req.Version }}/{{ .Req.Dir }}
GO19CONCURRENTCOMPILATION=0 GO111MODULE=on GOPROXY={{ .GoProxy }} \
	CGO_ENABLED=0 GOOS={{ .Req.Goos }} GOARCH={{ .Req.Goarch }} \
	{{ .Req.Goversion }} build -mod=readonly -o $tmpdir/{{ .DownloadFilename }} -x -v -trimpath -ldflags=-buildid=
sha256sum $tmpdir/{{ .DownloadFilename }}
{{ if .SHA256 }}# should be: {{ .SHA256 }}{{ end }}
</pre>

	<div style="width: 32%; display: inline-block; vertical-align: top">
		<h2>Module versions</h2>
	{{ if .Mod.Err }}
		<div>error: {{ .Mod.Err }}</div>
	{{ else }}
	{{ range .Mod.VersionLinks }}	<div><a href="{{ .URLPath }}" class="buildlink{{ if .Active }} active{{ end }} ">{{ .Version }}</a>{{ if .Available }} {{ if .Success }}<span class="success">✓</span>{{ else }}<span class="failure">❌</span>{{ end }}{{ end }}</div>{{ end }}
	{{ end }}
	</div>

	<div style="width: 32%; display: inline-block; vertical-align: top">
		<h2>Go versions</h2>
	{{ range .GoversionLinks }}	<div><a href="{{ .URLPath }}" class="buildlink{{ if .Active }} active{{ end }} {{ if not .Supported }} unsupported{{ end }}">{{ .Goversion }}</a>{{ if .Available }} {{ if .Success }}<span class="success">✓</span>{{ else }}<span class="failure">❌</span>{{ end }}{{ end }}</div>{{ end }}
	</div>

	<div style="width: 32%; display: inline-block; vertical-align: top">
		<h2>Targets</h2>
	{{ range .TargetLinks }}	<div><a href="{{ .URLPath }}" class="buildlink{{ if .Active }} active{{ end }} ">{{ .Goos }}/{{ .Goarch }}</a>{{ if .Available }} {{ if .Success }}<span class="success">✓</span>{{ else }}<span class="failure">❌</span>{{ end }}{{ end }}</div>{{ end }}
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

	const progress = document.getElementById('progress')
	function display() {
		while (progress.firstChild) {
			progress.removeChild(progress.firstChild)
		}
		var args = Array.prototype.slice.call(arguments)
		args.unshift('div')
		const e = elem.apply(undefined, args)
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
{{ define "title" }}Gobuild: Reproducible binaries for the go module proxy{{ end }}
{{ define "content" }}
		<h1>Gobuild: reproducible binaries with the go module proxy</h1>
		<p>Gobuild deterministically compiles programs written in Go that are available through the Go module proxy, and returns the binary.</p>

		<p>The <a href="https://proxy.golang.org/">Go module proxy</a> ensures source code stays available, and you are highly likely to get the same code each time you fetch it. Gobuild aims to do the same for binaries.</p>

		<h2>Try a module</h2>
		<form onsubmit="location.href = '/m/' + moduleName.value; return false" method="GET" action="/m/">
			<input id="moduleName" name="m" type="text" placeholder="github.com/your/project" style="width:30rem; max-width:75%" />
			<button type="submit">Go!</button>
		</form>

		<h2>Recent builds</h2>
		<div style="white-space: nowrap">
{{ if not .Recents }}<p>No builds yet.</p>{{ end }}
			<ul>
{{ range .Recents }}			<li><a href="{{ . }}">{{ . }}</a></li>{{ end }}
			</ul>
		</div>

		<h2>URLs</h2>
		<blockquote style="color:#666; white-space: nowrap">
			<div>/m/<span style="color:#111">&lt;module&gt;</span></div>
			<div>/b/<span style="color:#111">&lt;module&gt;</span>@<span style="color:#111">&lt;version&gt;</span>/<span style="color:#111">&lt;package&gt;</span>/<span style="color:#111">&lt;goos&gt;</span>-<span style="color:#111">&lt;goarch&gt;</span>-<span style="color:#111">&lt;goversion&gt;</span>/</div>
			<div>/r/<span style="color:#111">&lt;module&gt;</span>@<span style="color:#111">&lt;version&gt;</span>/<span style="color:#111">&lt;package&gt;</span>/<span style="color:#111">&lt;goos&gt;</span>-<span style="color:#111">&lt;goarch&gt;</span>-<span style="color:#111">&lt;goversion&gt;</span>/<span style="color:#111">&lt;sum&gt;</span>/</div>
		</blockquote>

		<h3>Examples</h3>
		<blockquote style="color:#666; white-space: nowrap">
			<a href="/m/github.com/mjl-/gobuild">/m/github.com/mjl-/gobuild</a><br/>
			<a href="/b/github.com/mjl-/sherpa@latest/cmd/sherpaclient/linux-amd64-latest/">/b/github.com/mjl-/sherpa@latest/cmd/sherpaclient/linux-amd64-latest/</a><br/>
			<a href="/r/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.14.1/0rLhZFgnc9hme13PhUpIvNw08LEk/">/r/github.com/mjl-/sherpa@v0.6.0/cmd/sherpaclient/linux-amd64-go1.14.1/0rLhZFgnc9hme13PhUpIvNw08LEk/</a>
		</blockquote>

		<p>The first URL fetches the requested Go module, and redirects to a URL of the second form.</p>
		<p>The second URL first resolves "latest" for the module and Go version with a redirect. Then starts a build for the requested parameters if needed. When finished, it redirects to a URL of the third kind.</p>
		<p>The third URL represents a successful build. The URL includes the sum: The versioned raw-base64-url-encoded 20-byte sha256-prefix. The page links to the binary, the build output log file, and to builds of the same package with different module versions, goversions, goos/goarch.</p>
		<p>You need not and cannot refresh a build: they would give the same result.</p>

		<h2>More</h2>
		<p>Only "go build" is run. None of "go test", "go generate", build tags, cgo, custom compile/link flags, makefiles, etc.</p>
		<p>Gobuild looks up module versions through the go proxy. That's why shorthand versions like "@v1" don't resolve.</p>
		<p>Code is available at <a href="https://github.com/mjl-/gobuild">github.com/mjl-/gobuild</a>, under MIT-license, feedback welcome.</p>
		<p>To build, gobuild executes:</p>
<pre>tmpdir=$(mktemp -d)
cd $tmpdir
GO111MODULE=on GOPROXY=https://proxy.golang.org/ $goversion get -d -v $module@$version
cd $HOME/go/pkg/mod/$module@$version/$path
GO19CONCURRENTCOMPILATION=0 GO111MODULE=on GOPROXY=https://proxy.golang.org/ \
	CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
	$goversion build -mod=readonly -o $tmpdir/$name -x -v -trimpath -ldflags=-buildid=</pre>
{{ end }}
{{ define "script" }}{{ end }}
`
