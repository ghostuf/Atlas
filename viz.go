package main

// Exports the dependency graph as a single self-contained HTML file
// (canvas + a small force-directed layout, no external libraries).

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

type vizNode struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Depth      int    `json:"depth"`
	Direct     bool   `json:"direct"`
	Root       bool   `json:"root"`
	Dependents int    `json:"dependents"`
	Transitive int    `json:"transitive"`
}

type vizData struct {
	Title string    `json:"title"`
	Total int       `json:"total"` // reachable packages in the full graph (excl. root)
	Nodes []vizNode `json:"nodes"`
	Edges [][2]int  `json:"edges"` // [from, to] = "from depends on to"
}

// writeViz keeps the root, every direct dependency, and then the packages with the
// largest blast radius until maxNodes is reached, so big graphs stay readable.
func writeViz(g *Graph, path string, maxNodes int) error {
	ids := []string{""} // root is always index 0
	var direct []string
	var rest []*Node
	total := 0
	for id, n := range g.Nodes {
		if id == "" || n.Depth < 0 {
			continue
		}
		total++
		if n.Direct {
			direct = append(direct, id)
		} else {
			rest = append(rest, n)
		}
	}
	sort.Strings(direct)
	ids = append(ids, direct...)

	sort.Slice(rest, func(i, j int) bool {
		if rest[i].TransitiveDependents != rest[j].TransitiveDependents {
			return rest[i].TransitiveDependents > rest[j].TransitiveDependents
		}
		return rest[i].ID < rest[j].ID
	})
	for i := 0; len(ids) < maxNodes && i < len(rest); i++ {
		ids = append(ids, rest[i].ID)
	}

	index := map[string]int{}
	for i, id := range ids {
		index[id] = i
	}

	data := vizData{Title: g.Root.Name, Total: total}
	for _, id := range ids {
		n := g.Nodes[id]
		data.Nodes = append(data.Nodes, vizNode{
			Name: n.Name, Version: n.Version, Depth: n.Depth, Direct: n.Direct,
			Root: id == "", Dependents: len(n.In), Transitive: n.TransitiveDependents,
		})
	}
	for _, id := range ids {
		for _, to := range g.Nodes[id].Out {
			if j, ok := index[to]; ok {
				data.Edges = append(data.Edges, [2]int{index[id], j})
			}
		}
	}

	b, err := json.Marshal(data) // escapes <, >, & so it is safe inside <script>
	if err != nil {
		return err
	}
	html := strings.Replace(vizTemplate, "__DATA__", string(b), 1)
	return os.WriteFile(path, []byte(html), 0o644)
}

const vizTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Atlas dependency graph</title>
<style>
:root{--bg:#ffffff;--fg:#1f2328;--muted:#6b7280;--panel:#f6f8fa;--line:#d0d7de;--edge:rgba(110,118,129,.40);--root:#d97706;--direct:#2563eb;--trans:#9ca3af;--hot:#dc2626}
@media (prefers-color-scheme:dark){:root{--bg:#0d1117;--fg:#e6edf3;--muted:#8b949e;--panel:#161b22;--line:#30363d;--edge:rgba(160,170,190,.30);--root:#f59e0b;--direct:#60a5fa;--trans:#6b7280;--hot:#f87171}}
html,body{margin:0;height:100%;overflow:hidden;background:var(--bg);color:var(--fg);font:14px/1.4 system-ui,-apple-system,"Segoe UI",sans-serif}
#c{position:absolute;left:0;top:0;width:100%;height:100%;cursor:grab;touch-action:none}
#panel{position:absolute;top:12px;left:12px;width:290px;background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:12px}
#panel h1{font-size:15px;margin:0 0 2px}
#meta{color:var(--muted);font-size:12px;margin-bottom:10px}
#q{width:100%;box-sizing:border-box;padding:6px 8px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--fg);font:inherit}
.legend{display:flex;flex-wrap:wrap;gap:4px 12px;margin:10px 0;font-size:12px;color:var(--muted)}
.dot{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:5px;vertical-align:-1px}
#info{border-top:1px solid var(--line);padding-top:10px;font-size:13px}
#info .t{font-weight:600;word-break:break-all;margin-bottom:6px}
#info .r{display:flex;justify-content:space-between;gap:12px;color:var(--muted)}
#info .r b{color:var(--fg);font-weight:600}
#info .hint{color:var(--muted)}
</style>
</head>
<body>
<canvas id="c"></canvas>
<div id="panel">
  <h1>Dependency graph</h1>
  <div id="meta"></div>
  <input id="q" type="search" placeholder="Search package..." autocomplete="off">
  <div class="legend">
    <span><span class="dot" style="background:var(--root)"></span>your app</span>
    <span><span class="dot" style="background:var(--direct)"></span>direct</span>
    <span><span class="dot" style="background:var(--trans)"></span>transitive</span>
    <span><span class="dot" style="background:var(--hot)"></span>affected</span>
  </div>
  <div id="info"></div>
</div>
<script>
(function(){
var DATA = __DATA__;
var nodes = DATA.nodes, edges = DATA.edges, N = nodes.length;
var out = nodes.map(function(){return [];}), inn = nodes.map(function(){return [];});
edges.forEach(function(e){ out[e[0]].push(e[1]); inn[e[1]].push(e[0]); });

document.getElementById('meta').textContent =
  DATA.title + ' \u2014 showing ' + (N-1) + ' of ' + DATA.total + ' packages. Arrows point from a package to what it depends on.';

var cv = document.getElementById('c'), ctx = cv.getContext('2d');
var W = 0, H = 0, dpr = 1, view = {x:0, y:0, k:1};
function resize(){
  dpr = window.devicePixelRatio || 1; W = window.innerWidth; H = window.innerHeight;
  cv.width = Math.round(W*dpr); cv.height = Math.round(H*dpr);
}
window.addEventListener('resize', resize); resize();

var col = {};
function loadColors(){
  var s = getComputedStyle(document.documentElement);
  function g(n){ return s.getPropertyValue(n).trim(); }
  col = {bg:g('--bg'), fg:g('--fg'), edge:g('--edge'), root:g('--root'), direct:g('--direct'), trans:g('--trans'), hot:g('--hot')};
}
loadColors();
if (window.matchMedia) window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', loadColors);

// ---- layout: force-directed, with a radial pull so depth = distance from your app ----
var RING = 110;
nodes.forEach(function(n){
  var a = Math.random()*Math.PI*2, r = n.depth*RING + (Math.random()-0.5)*40;
  n.x = n.root ? 0 : Math.cos(a)*r; n.y = n.root ? 0 : Math.sin(a)*r;
  n.vx = 0; n.vy = 0; n.fixed = false;
  n.r = n.root ? 12 : Math.min(26, 4 + 2*Math.sqrt(n.transitive));
});
var alpha = 1;
function tick(){
  if (alpha < 0.02) return;
  var i, j, a, b, dx, dy, d2, d, f;
  for (i = 0; i < N; i++){
    a = nodes[i];
    for (j = i+1; j < N; j++){
      b = nodes[j]; dx = a.x-b.x; dy = a.y-b.y; d2 = dx*dx + dy*dy;
      if (d2 > 160000) continue;
      if (d2 < 100) d2 = 100;
      f = 500/d2*alpha;
      a.vx += dx*f; a.vy += dy*f; b.vx -= dx*f; b.vy -= dy*f;
    }
  }
  for (i = 0; i < edges.length; i++){
    a = nodes[edges[i][0]]; b = nodes[edges[i][1]];
    dx = b.x-a.x; dy = b.y-a.y; d = Math.sqrt(dx*dx + dy*dy) || 1;
    f = (d-70)*0.03*alpha;
    a.vx += dx/d*f; a.vy += dy/d*f; b.vx -= dx/d*f; b.vy -= dy/d*f;
  }
  for (i = 0; i < N; i++){
    a = nodes[i]; d = Math.sqrt(a.x*a.x + a.y*a.y) || 1;
    f = (a.depth*RING - d)*0.02*alpha;
    a.vx += a.x/d*f; a.vy += a.y/d*f;
  }
  for (i = 0; i < N; i++){
    a = nodes[i];
    if (a.root && !a.fixed){ a.x = 0; a.y = 0; a.vx = 0; a.vy = 0; continue; }
    if (a.fixed){ a.vx = 0; a.vy = 0; continue; }
    a.vx *= 0.8; a.vy *= 0.8; a.x += a.vx; a.y += a.vy;
  }
  alpha *= 0.992;
}

function fit(){
  var x0 = 1e9, y0 = 1e9, x1 = -1e9, y1 = -1e9;
  nodes.forEach(function(n){
    x0 = Math.min(x0, n.x-n.r); y0 = Math.min(y0, n.y-n.r);
    x1 = Math.max(x1, n.x+n.r); y1 = Math.max(y1, n.y+n.r);
  });
  var w = Math.max(x1-x0, 1), h = Math.max(y1-y0, 1);
  view.k = Math.min(W/w, H/h)*0.9;
  view.x = W/2 - (x0 + w/2)*view.k; view.y = H/2 - (y0 + h/2)*view.k;
}

// ---- selection: reverse reachability = blast radius ----
var sel = -1, hot = new Set(), hover = -1;
function select(i){
  sel = i; hot = new Set();
  if (i >= 0){
    var st = [i]; hot.add(i);
    while (st.length){
      var c = st.pop();
      for (var k = 0; k < inn[c].length; k++){
        var p = inn[c][k];
        if (!hot.has(p)){ hot.add(p); st.push(p); }
      }
    }
  }
  updateInfo();
}
function row(el, label, value){
  var r = document.createElement('div'); r.className = 'r';
  var l = document.createElement('span'); l.textContent = label;
  var v = document.createElement('b'); v.textContent = value;
  r.appendChild(l); r.appendChild(v); el.appendChild(r);
}
function updateInfo(){
  var el = document.getElementById('info'); el.textContent = '';
  if (sel < 0){
    var h = document.createElement('div'); h.className = 'hint';
    h.textContent = 'Click a package to highlight everything that would be affected if it broke. Drag nodes, scroll to zoom, drag the background to pan.';
    el.appendChild(h); return;
  }
  var n = nodes[sel];
  var t = document.createElement('div'); t.className = 't';
  t.textContent = n.name + (n.version ? '@' + n.version : '');
  el.appendChild(t);
  var affected = hot.size - 1 - ((hot.has(0) && sel !== 0) ? 1 : 0);
  row(el, 'Depth', n.root ? '-' : String(n.depth));
  row(el, 'Direct dependency', n.direct ? 'yes' : 'no');
  row(el, 'Used directly by', String(n.dependents) + ' packages');
  row(el, 'Affected if it breaks', String(n.transitive) + ' packages');
  row(el, 'Highlighted here', String(Math.max(affected, 0)));
}

// ---- drawing ----
function drawEdge(e, isHot){
  var a = nodes[e[0]], b = nodes[e[1]];
  var dx = b.x-a.x, dy = b.y-a.y, d = Math.sqrt(dx*dx + dy*dy);
  if (d < 1) return;
  var ux = dx/d, uy = dy/d;
  var sx = a.x + ux*a.r, sy = a.y + uy*a.r, tx = b.x - ux*b.r, ty = b.y - uy*b.r;
  ctx.beginPath(); ctx.moveTo(sx, sy); ctx.lineTo(tx, ty); ctx.stroke();
  var s = (isHot ? 8 : 6)/view.k;
  ctx.beginPath();
  ctx.moveTo(tx, ty);
  ctx.lineTo(tx - ux*s - uy*s*0.4, ty - uy*s + ux*s*0.4);
  ctx.lineTo(tx - ux*s + uy*s*0.4, ty - uy*s - ux*s*0.4);
  ctx.closePath(); ctx.fill();
}
function draw(){
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.globalAlpha = 1; ctx.fillStyle = col.bg; ctx.fillRect(0, 0, W, H);
  ctx.save(); ctx.translate(view.x, view.y); ctx.scale(view.k, view.k);
  var dim = sel >= 0, i, e, isHot, pass;
  for (pass = 0; pass < 2; pass++){
    for (i = 0; i < edges.length; i++){
      e = edges[i];
      isHot = dim && hot.has(e[0]) && hot.has(e[1]);
      if ((pass === 1) !== isHot) continue;
      ctx.globalAlpha = (dim && !isHot) ? 0.08 : 1;
      ctx.strokeStyle = isHot ? col.hot : col.edge; ctx.fillStyle = ctx.strokeStyle;
      ctx.lineWidth = (isHot ? 1.6 : 1)/view.k;
      drawEdge(e, isHot);
    }
  }
  for (i = 0; i < N; i++){
    var n = nodes[i]; isHot = dim && hot.has(i);
    ctx.globalAlpha = (dim && !isHot) ? 0.15 : 1;
    ctx.beginPath(); ctx.arc(n.x, n.y, n.r, 0, Math.PI*2);
    ctx.fillStyle = n.root ? col.root : (n.direct ? col.direct : col.trans); ctx.fill();
    if (isHot && i !== sel){ ctx.lineWidth = 2.5/view.k; ctx.strokeStyle = col.hot; ctx.stroke(); }
    if (i === sel){ ctx.lineWidth = 3/view.k; ctx.strokeStyle = col.fg; ctx.stroke(); }
  }
  ctx.font = (11/view.k) + 'px system-ui, sans-serif'; ctx.textAlign = 'center'; ctx.textBaseline = 'top';
  for (i = 0; i < N; i++){
    var m = nodes[i];
    var show = m.root || m.direct || i === hover || i === sel || (dim && hot.has(i)) || view.k > 1.6 || m.transitive >= 5;
    if (!show) continue;
    ctx.globalAlpha = (dim && !hot.has(i)) ? 0.2 : 1; ctx.fillStyle = col.fg;
    ctx.fillText(m.name, m.x, m.y + m.r + 2/view.k);
  }
  ctx.restore(); ctx.globalAlpha = 1;
}

// ---- interaction ----
function pick(x, y){
  var wx = (x-view.x)/view.k, wy = (y-view.y)/view.k;
  for (var i = N-1; i >= 0; i--){
    var n = nodes[i], dx = wx-n.x, dy = wy-n.y, r = n.r + 3/view.k;
    if (dx*dx + dy*dy <= r*r) return i;
  }
  return -1;
}
var drag = -1, pan = false, movedDist = 0, last = {x:0, y:0};
cv.addEventListener('pointerdown', function(e){
  if (cv.setPointerCapture) cv.setPointerCapture(e.pointerId);
  movedDist = 0; last = {x:e.clientX, y:e.clientY};
  var i = pick(e.clientX, e.clientY);
  if (i >= 0){ drag = i; nodes[i].fixed = true; } else { pan = true; }
  cv.style.cursor = 'grabbing';
});
cv.addEventListener('pointermove', function(e){
  var dx = e.clientX-last.x, dy = e.clientY-last.y;
  if (drag >= 0){
    movedDist += Math.abs(dx) + Math.abs(dy);
    nodes[drag].x = (e.clientX-view.x)/view.k; nodes[drag].y = (e.clientY-view.y)/view.k;
    alpha = Math.max(alpha, 0.3);
  } else if (pan){
    movedDist += Math.abs(dx) + Math.abs(dy);
    view.x += dx; view.y += dy;
  } else {
    hover = pick(e.clientX, e.clientY);
    cv.style.cursor = hover >= 0 ? 'pointer' : 'grab';
  }
  last = {x:e.clientX, y:e.clientY};
});
function release(){
  if (movedDist < 4){ select(drag >= 0 ? drag : -1); }
  if (drag >= 0) nodes[drag].fixed = false;
  drag = -1; pan = false; cv.style.cursor = 'grab';
}
cv.addEventListener('pointerup', release);
cv.addEventListener('pointercancel', release);
cv.addEventListener('wheel', function(e){
  e.preventDefault();
  var k0 = view.k, k = Math.min(6, Math.max(0.05, k0*Math.exp(-e.deltaY*0.0015)));
  var wx = (e.clientX-view.x)/k0, wy = (e.clientY-view.y)/k0;
  view.k = k; view.x = e.clientX - wx*k; view.y = e.clientY - wy*k;
}, {passive:false});
document.getElementById('q').addEventListener('input', function(){
  var q = this.value.trim().toLowerCase();
  if (!q){ select(-1); return; }
  var best = -1;
  for (var i = 0; i < N; i++){
    if (nodes[i].name.toLowerCase().indexOf(q) >= 0 && (best < 0 || nodes[i].transitive > nodes[best].transitive)) best = i;
  }
  if (best >= 0){ select(best); view.x = W/2 - nodes[best].x*view.k; view.y = H/2 - nodes[best].y*view.k; }
});

// settle the layout before the first frame, then fit to screen
for (var s = 0; s < 450; s++) tick();
fit(); updateInfo();
function loop(){ tick(); draw(); window.requestAnimationFrame(loop); }
loop();
})();
</script>
</body>
</html>
`
