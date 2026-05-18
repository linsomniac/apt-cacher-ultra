package admin

import (
	_ "embed"
	"encoding/base64"
	"html/template"
	"strings"
)

// Geist Sans + Geist Mono variable woff2, subset to Latin-1 plus
// the arrows / box-drawing / dot glyphs the template actually uses.
// Combined binary ~52KB, base64 ~70KB. OFL via Vercel.
//
//go:embed fonts/geist-sans.woff2
var geistSansWOFF2 []byte

//go:embed fonts/geist-mono.woff2
var geistMonoWOFF2 []byte

// statusHTMLTemplate renders the SPEC5 §10.5 / docs/admin-ui-spec.md
// status page. The raw template carries __GEIST_*_B64__ placeholders
// where the inline @font-face data URI lives; buildStatusHTML
// substitutes them once at init from the embedded woff2 above so the
// rendered HTML is fully self-contained (zero external requests, the
// air-gap requirement in docs/admin-ui-spec.md §0).
//
// html/template auto-escapes every interpolated value; never switch
// to text/template without a full security review. The data-*
// attribute contract this template emits is read by the inline JS at
// the bottom (verdict pill, aggregate-failure notice, theme toggle,
// sticky-rail highlight, time localization). Keep markup and JS in
// sync in the same change.
//
// AIDEV-NOTE: visual ground truth is docs/admin-ui-mockup-v2.html
// (the INSTRUMENT direction). The mockup wins when it disagrees with
// this template's structure. See docs/admin-ui-redesign-plan.md for
// the design rationale.
var statusHTMLTemplate = template.Must(
	template.New("status").Funcs(statusTemplateFuncMap()).Parse(buildStatusHTML()),
)

func buildStatusHTML() string {
	s := statusHTMLRaw
	s = strings.Replace(s, "__GEIST_SANS_B64__",
		base64.StdEncoding.EncodeToString(geistSansWOFF2), 1)
	s = strings.Replace(s, "__GEIST_MONO_B64__",
		base64.StdEncoding.EncodeToString(geistMonoWOFF2), 1)
	return s
}

const statusHTMLRaw = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="60">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>apt-cacher-ultra status</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Crect width='16' height='16' fill='%239A4810'/%3E%3Cpath d='M3 4h10v8H3z' fill='none' stroke='%23F4F2EC' stroke-width='1'/%3E%3Cpath d='M4 8h2M7 8h2M10 8h2' stroke='%23F4F2EC' stroke-width='1.4' stroke-linecap='square'/%3E%3C/svg%3E">
<script>(function(){try{var s=localStorage.getItem('acu-theme');if(s==='light'||s==='dark')document.documentElement.setAttribute('data-theme',s);}catch(e){}})();</script>
<style>
/* apt-cacher-ultra — INSTRUMENT theme.
   Fonts: Geist Sans + Geist Mono variable, OFL via Vercel, subset to
   Latin-1 + arrows + box-drawing (~52KB binary inline). */
@font-face{font-family:'Geist';src:url(data:font/woff2;base64,__GEIST_SANS_B64__) format('woff2');font-weight:100 900;font-style:normal;font-display:block}
@font-face{font-family:'Geist Mono';src:url(data:font/woff2;base64,__GEIST_MONO_B64__) format('woff2');font-weight:100 900;font-style:normal;font-display:block}
:root{
--bg:#F4F2EC;--bg-deep:#ECE9DF;--bg-panel:#FAF8F2;--bg-row:rgba(0,0,0,0.022);
--rule:#D6D3C7;--rule-hard:#ADA995;
--ink-low:#6A675B;--ink-mid:#2E2B25;--ink-high:#0E1013;
--signal:#9A4810;--ok:#2F5D3E;--warn:#925208;--crit:#B5311C;--stale:#6A675B;
--tint-warn:rgba(146,82,8,0.06);--tint-crit:rgba(181,49,28,0.055);--tint-ok:rgba(47,93,62,0.045);
--shadow-engrave:inset 0 1px 0 rgba(255,255,255,0.6),inset 0 -1px 0 rgba(0,0,0,0.04);
--font-sans:'Geist',ui-sans-serif,system-ui,-apple-system,sans-serif;
--font-mono:'Geist Mono',ui-monospace,'SF Mono',Menlo,Consolas,monospace;
--bar-h:64px;--max-w:1440px;--rail-w:220px;
--xs:10.5px;--sm:12.5px;--base:14px;--md:16px;--lg:20px;--xl:32px;
--r-pill:999px;--r-badge:2px;
}
@media (prefers-color-scheme:dark){:root{
--bg:#0D1014;--bg-deep:#07090C;--bg-panel:#14181D;--bg-row:rgba(255,255,255,0.026);
--rule:#232830;--rule-hard:#3D434C;
--ink-low:#7E8388;--ink-mid:#C0C3C8;--ink-high:#EAEDEF;
--signal:#FFB13C;--ok:#7FBC8F;--warn:#E4A33B;--crit:#E66B5E;--stale:#7E8388;
--tint-warn:rgba(228,163,59,0.07);--tint-crit:rgba(230,107,94,0.07);--tint-ok:rgba(127,188,143,0.05);
--shadow-engrave:inset 0 1px 0 rgba(255,255,255,0.04),inset 0 -1px 0 rgba(0,0,0,0.4);
}}
[data-theme="light"]{--bg:#F4F2EC;--bg-deep:#ECE9DF;--bg-panel:#FAF8F2;--bg-row:rgba(0,0,0,0.022);--rule:#D6D3C7;--rule-hard:#ADA995;--ink-low:#6A675B;--ink-mid:#2E2B25;--ink-high:#0E1013;--signal:#9A4810;--ok:#2F5D3E;--warn:#925208;--crit:#B5311C;--stale:#6A675B;--tint-warn:rgba(146,82,8,0.06);--tint-crit:rgba(181,49,28,0.055);--tint-ok:rgba(47,93,62,0.045);--shadow-engrave:inset 0 1px 0 rgba(255,255,255,0.6),inset 0 -1px 0 rgba(0,0,0,0.04)}
[data-theme="dark"]{--bg:#0D1014;--bg-deep:#07090C;--bg-panel:#14181D;--bg-row:rgba(255,255,255,0.026);--rule:#232830;--rule-hard:#3D434C;--ink-low:#7E8388;--ink-mid:#C0C3C8;--ink-high:#EAEDEF;--signal:#FFB13C;--ok:#7FBC8F;--warn:#E4A33B;--crit:#E66B5E;--stale:#7E8388;--tint-warn:rgba(228,163,59,0.07);--tint-crit:rgba(230,107,94,0.07);--tint-ok:rgba(127,188,143,0.05);--shadow-engrave:inset 0 1px 0 rgba(255,255,255,0.04),inset 0 -1px 0 rgba(0,0,0,0.4)}
*{box-sizing:border-box}
html,body{margin:0;padding:0;background:var(--bg);color:var(--ink-mid);font-family:var(--font-sans);font-size:var(--base);line-height:1.55;font-feature-settings:'ss01','ss03','tnum';-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
body{background-image:radial-gradient(circle at 1px 1px,color-mix(in srgb,var(--ink-low) 9%,transparent) 1px,transparent 1px);background-size:8px 8px}
a{color:var(--signal);text-decoration:none;border-bottom:1px solid transparent;transition:border-color 120ms ease,color 120ms ease}
a:hover{border-bottom-color:var(--signal)}
a:focus-visible{outline:2px solid var(--signal);outline-offset:2px;border-radius:1px}
code,.mono{font-family:var(--font-mono);font-feature-settings:'tnum','zero','ss01'}
.bar{position:sticky;top:0;z-index:50;height:var(--bar-h);background:var(--bg);background-image:linear-gradient(180deg,color-mix(in srgb,var(--bg) 92%,var(--ink-high)) 0%,var(--bg) 60%);border-bottom:1px solid var(--rule-hard);display:flex;align-items:stretch;padding:0 24px;box-shadow:0 1px 0 0 color-mix(in srgb,var(--ink-high) 4%,transparent)}
.plate{display:flex;align-items:center;gap:14px;padding:10px 18px 10px 4px;margin:8px 24px 8px 0;border-right:1px solid var(--rule);position:relative}
.plate__mark{display:inline-flex;align-items:center;justify-content:center;width:38px;height:38px;background:var(--signal);color:var(--bg);font-family:var(--font-mono);font-weight:700;font-size:22px;line-height:1;letter-spacing:-0.04em;position:relative;box-shadow:var(--shadow-engrave),inset 0 0 0 1px color-mix(in srgb,var(--ink-high) 18%,transparent)}
.plate__mark::before{content:'';position:absolute;top:3px;left:3px;right:3px;bottom:3px;border:1px solid color-mix(in srgb,var(--bg) 55%,transparent);pointer-events:none}
.plate__name{font-family:var(--font-mono);font-weight:600;font-size:13.5px;letter-spacing:0.06em;text-transform:uppercase;color:var(--ink-high);white-space:nowrap}
.plate__sn{display:inline-flex;flex-direction:column;gap:1px;font-family:var(--font-mono);font-size:9px;line-height:1.1;letter-spacing:0.18em;color:var(--ink-low);text-transform:uppercase;border-left:1px solid var(--rule);padding-left:12px}
.plate__sn b{color:var(--ink-mid);font-weight:500;letter-spacing:0.08em;font-size:10.5px}
.verdict{display:flex;align-items:center;gap:16px;flex:1;min-width:0;padding:0 4px}
.verdict__pill{display:inline-flex;align-items:center;gap:8px;height:28px;padding:0 14px 0 10px;font-family:var(--font-mono);font-weight:600;font-size:11px;letter-spacing:0.15em;text-transform:uppercase;white-space:nowrap;border:1px solid currentColor;border-radius:var(--r-pill);background:color-mix(in srgb,currentColor 10%,transparent)}
.verdict__pill[data-state="ok"]{color:var(--ok)}
.verdict__pill[data-state="warn"]{color:var(--warn)}
.verdict__pill[data-state="crit"]{color:var(--crit)}
.verdict__pill[data-state="stale"]{color:var(--stale)}
.verdict__dot{width:8px;height:8px;background:currentColor;border-radius:50%;box-shadow:0 0 0 2px color-mix(in srgb,currentColor 22%,transparent);animation:pulse 2.4s ease-in-out infinite}
@keyframes pulse{0%,100%{opacity:1}50%{opacity:0.45}}
.verdict__msg{color:var(--ink-low);font-size:var(--sm);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;letter-spacing:0.01em}
.verdict__msg b{color:var(--ink-mid);font-weight:600}
.meta{display:flex;align-items:center;gap:12px;font-family:var(--font-mono);font-size:10.5px;letter-spacing:0.12em;text-transform:uppercase;color:var(--ink-low)}
.meta .sep{color:var(--rule)}
.meta__val{color:var(--ink-mid);font-weight:500;letter-spacing:0.06em}
.chip{display:inline-flex;align-items:center;gap:6px;height:26px;padding:0 10px;font-family:var(--font-mono);font-size:10px;letter-spacing:0.15em;text-transform:uppercase;color:var(--ink-low);border:1px solid var(--rule);border-bottom:0 !important;transition:color 120ms ease,border-color 120ms ease,background 120ms ease}
.chip:hover{color:var(--ink-high);border-color:var(--ink-low);background:var(--bg-panel)}
.chip__count{color:var(--ink-high);font-weight:600;letter-spacing:0;font-size:11.5px}
.chip__ok{color:var(--ok);font-size:11px}
.chip[data-state="crit"]{color:var(--crit);border-color:var(--crit)}
.chip[data-state="crit"] .chip__count{color:var(--crit)}
.chip[data-state="crit"] .chip__ok{color:var(--crit)}
.icon-btn{width:26px;height:26px;display:inline-flex;align-items:center;justify-content:center;border:1px solid var(--rule);background:var(--bg-panel);color:var(--ink-low);cursor:pointer;transition:color 120ms ease,background 120ms ease,border-color 120ms ease}
.icon-btn:hover{color:var(--ink-high);background:var(--bg);border-color:var(--ink-low)}
.icon-btn svg{width:13px;height:13px}
a.icon-btn{text-decoration:none;border-bottom:0 !important}
.page{max-width:var(--max-w);margin:0 auto;padding:32px 32px 96px}
.vitals{display:grid;grid-template-columns:repeat(5,minmax(0,1fr));margin-bottom:56px;background:var(--bg-deep);border:1px solid var(--rule-hard);box-shadow:var(--shadow-engrave);position:relative}
.vitals::before{content:'';position:absolute;left:0;right:0;top:-1px;height:1px;background:linear-gradient(90deg,transparent 0%,color-mix(in srgb,var(--signal) 50%,transparent) 12%,color-mix(in srgb,var(--signal) 80%,transparent) 50%,color-mix(in srgb,var(--signal) 50%,transparent) 88%,transparent 100%);opacity:0.55}
.vital{position:relative;padding:16px 18px 20px;border-right:1px solid var(--rule);display:flex;flex-direction:column;background:var(--bg-panel);min-height:144px}
.vital:last-child{border-right:0}
.vital::before{content:'';position:absolute;left:0;top:0;bottom:0;width:3px;background:var(--rule);transition:background 200ms ease}
.vital[data-state="ok"]::before{background:var(--ok)}
.vital[data-state="warn"]::before{background:var(--warn)}
.vital[data-state="crit"]::before{background:var(--crit)}
.vital[data-state="stale"]::before{background:var(--stale)}
.vital__head{display:flex;align-items:baseline;justify-content:space-between;font-family:var(--font-mono);font-size:10.5px;letter-spacing:0.2em;text-transform:uppercase;color:var(--signal);font-weight:600;margin-bottom:16px}
.vital__label{color:var(--signal);font-weight:600}
.vital__readout{display:flex;align-items:baseline;gap:8px;color:var(--ink-high)}
.vital__num{font-family:var(--font-sans);font-weight:500;font-size:var(--xl);line-height:1;letter-spacing:-0.02em;font-variant-numeric:tabular-nums}
.vital__unit{font-family:var(--font-mono);font-size:11.5px;letter-spacing:0.06em;color:var(--ink-low);font-weight:500;text-transform:uppercase}
.vital__num.muted{color:var(--ink-low)}
.vital__sub{display:flex;flex-direction:column;gap:3px;margin-top:12px;font-family:var(--font-mono);font-size:10.5px;letter-spacing:0.06em;text-transform:uppercase;color:var(--ink-low)}
.vital__sub b{color:var(--ink-mid);font-weight:500}
.vital__sub .accent-warn{color:var(--warn);font-weight:500}
.vital__sub .accent-crit{color:var(--crit);font-weight:500}
.vital__sub .accent-ok{color:var(--ok);font-weight:500}
.layout{display:grid;grid-template-columns:var(--rail-w) 1fr;gap:56px;align-items:start}
.rail{position:sticky;top:calc(var(--bar-h) + 24px);font-family:var(--font-mono)}
.rail__head{font-size:9.5px;letter-spacing:0.2em;text-transform:uppercase;color:var(--ink-low);margin:0 0 14px 0;padding-bottom:8px;border-bottom:1px solid var(--rule-hard);display:flex;align-items:baseline;justify-content:space-between}
.rail ul{list-style:none;margin:0;padding:0;display:flex;flex-direction:column}
.rail li{position:relative}
.rail a{display:block;padding:7px 0;font-size:11px;letter-spacing:0.14em;text-transform:uppercase;color:var(--ink-low);border-bottom:0 !important;transition:color 120ms ease}
.rail a:hover{color:var(--ink-high)}
.rail a.is-active,.rail a[aria-current="location"]{color:var(--ink-high)}
.rail a.is-active::before,.rail a[aria-current="location"]::before{content:'';position:absolute;left:-16px;top:50%;transform:translateY(-50%);width:3px;height:14px;background:var(--signal)}
.content{min-width:0}
.panel{margin-bottom:56px;scroll-margin-top:calc(var(--bar-h) + 16px)}
.panel__eyebrow{display:flex;align-items:baseline;gap:12px;flex-wrap:wrap;font-family:var(--font-mono);font-size:10px;letter-spacing:0.2em;text-transform:uppercase;color:var(--ink-low);margin-bottom:6px;padding-bottom:10px;border-bottom:1px dashed var(--rule)}
.panel__eyebrow .sep{color:var(--rule)}
.panel__eyebrow .count-ok{color:var(--ok);font-weight:600}
.panel__eyebrow .count-warn{color:var(--warn);font-weight:600}
.panel__eyebrow .count-crit{color:var(--crit);font-weight:600}
.panel__h{font-family:var(--font-sans);font-size:var(--lg);font-weight:500;letter-spacing:-0.015em;color:var(--ink-high);margin:6px 0 8px 0}
.panel__desc{font-size:var(--sm);color:var(--ink-low);margin:0 0 18px 0;max-width:70ch;line-height:1.65}
.panel__desc code{color:var(--ink-mid);background:var(--bg-deep);padding:1px 6px;font-size:11.5px;border:1px solid var(--rule)}
.table-wrap{border:1px solid var(--rule);background:var(--bg-panel);overflow-x:auto;box-shadow:var(--shadow-engrave)}
table.data{width:100%;border-collapse:collapse;font-variant-numeric:tabular-nums;font-size:var(--sm);color:var(--ink-mid)}
table.data thead th{background:var(--bg-deep);text-align:left;font-family:var(--font-mono);font-size:10px;font-weight:500;letter-spacing:0.18em;text-transform:uppercase;color:var(--ink-low);padding:10px 16px;border-bottom:1px solid var(--rule-hard);white-space:nowrap}
table.data tbody td{padding:9px 16px;border-bottom:1px solid var(--rule);vertical-align:middle;white-space:nowrap;transition:background 120ms ease}
table.data tbody tr:last-child td{border-bottom:0}
table.data tbody tr:nth-child(even) td{background:var(--bg-row)}
table.data tbody tr:hover td{background:color-mix(in srgb,var(--signal) 6%,transparent)}
table.data .num{text-align:right;font-family:var(--font-mono)}
table.data .mono{font-family:var(--font-mono);font-size:12px;color:var(--ink-mid)}
table.data .host{font-family:var(--font-mono);font-size:12px;color:var(--ink-high);font-weight:500;letter-spacing:0.02em}
table.data .muted{color:var(--ink-low)}
table.data .time{font-family:var(--font-mono);font-size:12px;color:var(--ink-mid);letter-spacing:0.02em}
table.data tbody tr[data-state="warn"] td{background:var(--tint-warn)}
table.data tbody tr[data-state="crit"] td{background:var(--tint-crit)}
table.data tbody tr[data-state="warn"] td:first-child{box-shadow:inset 3px 0 0 0 var(--warn)}
table.data tbody tr[data-state="crit"] td:first-child{box-shadow:inset 3px 0 0 0 var(--crit)}
table.data tbody tr[data-state="warn"]:hover td{background:color-mix(in srgb,var(--warn) 10%,transparent)}
table.data tbody tr[data-state="crit"]:hover td{background:color-mix(in srgb,var(--crit) 10%,transparent)}
.lagging{margin-left:8px;font-family:var(--font-mono);font-size:10.5px;letter-spacing:0.05em;color:var(--warn);font-variant-numeric:tabular-nums}
.b{display:inline-block;padding:1px 7px 2px;font-family:var(--font-mono);font-size:9.5px;letter-spacing:0.14em;font-weight:500;text-transform:uppercase;border:1px solid currentColor;border-radius:var(--r-badge);background:transparent;line-height:1.5;white-space:nowrap}
.b--ok{color:var(--ok)}.b--warn{color:var(--warn)}.b--crit{color:var(--crit)}.b--stale{color:var(--stale)}
.b--neutral{color:var(--ink-low);border-color:var(--rule-hard)}
.b--signal{color:var(--signal)}
.b.reason{margin-left:6px;letter-spacing:0.08em}
.notice-mount{display:contents}
.notice{position:relative;border:1px solid var(--crit);background:var(--tint-crit);padding:14px 18px 16px 22px;margin-bottom:24px;display:flex;flex-direction:column;gap:10px}
.notice::before{content:'';position:absolute;left:0;top:0;bottom:0;width:4px;background:var(--crit)}
.notice__head{display:flex;align-items:center;gap:12px;font-family:var(--font-mono);font-size:11px;font-weight:600;letter-spacing:0.15em;text-transform:uppercase;color:var(--crit)}
.notice__head .icon{font-size:14px;line-height:1}
.notice__head .b{color:var(--crit)}
.notice__body{font-size:var(--sm);color:var(--ink-mid);line-height:1.7}
.notice__body code{background:var(--bg-panel);padding:1px 6px;border:1px solid var(--rule);color:var(--ink-high);font-size:12px}
.notice__kv{display:grid;grid-template-columns:max-content 1fr;gap:4px 16px;font-family:var(--font-mono);font-size:11px;letter-spacing:0.06em;text-transform:uppercase;color:var(--ink-low)}
.notice__kv b{color:var(--ink-high);font-weight:500}
.notice__link{margin-top:6px;padding-top:10px;border-top:1px dashed var(--rule);font-family:var(--font-mono);font-size:11px;letter-spacing:0.12em;text-transform:uppercase;color:var(--ink-low)}
.notice__link a{color:var(--signal);font-weight:600}
.notice--warn{border-color:var(--warn);background:var(--tint-warn)}
.notice--warn::before{background:var(--warn)}
.notice--warn .notice__head{color:var(--warn)}
.kv{display:grid;grid-template-columns:minmax(220px,30%) 1fr;background:var(--bg-panel);border:1px solid var(--rule);box-shadow:var(--shadow-engrave)}
.kv > div{padding:10px 16px;border-bottom:1px solid var(--rule)}
.kv > div:nth-last-child(-n+2){border-bottom:0}
.kv .k{font-family:var(--font-mono);font-size:10.5px;letter-spacing:0.14em;text-transform:uppercase;color:var(--ink-low);text-align:right;border-right:1px solid var(--rule)}
.kv .v{font-size:var(--sm);color:var(--ink-mid);word-break:break-word}
.kv .v code{color:var(--ink-high);font-size:12px;background:var(--bg-deep);padding:1px 6px;border:1px solid var(--rule)}
.kv .v.muted{color:var(--ink-low)}
.kv__group-head{grid-column:1 / -1;background:var(--bg-deep);padding:9px 16px;font-family:var(--font-mono);font-size:10px;letter-spacing:0.2em;text-transform:uppercase;color:var(--ink-low);border-bottom:1px solid var(--rule-hard) !important;display:flex;align-items:baseline;gap:12px}
.kv__group-head .sep{color:var(--rule)}
.empty{border:1px solid var(--rule);background:var(--bg-panel);padding:36px 24px;text-align:center;position:relative;box-shadow:var(--shadow-engrave)}
.empty::before,.empty::after{content:'';position:absolute;width:14px;height:14px;border:1px solid var(--rule-hard)}
.empty::before{top:-1px;left:-1px;border-right:0;border-bottom:0}
.empty::after{bottom:-1px;right:-1px;border-left:0;border-top:0}
.empty__head{font-family:var(--font-mono);font-size:11px;letter-spacing:0.22em;text-transform:uppercase;color:var(--stale);font-weight:600;margin-bottom:8px}
.empty--crit .empty__head{color:var(--crit)}
.empty--crit{border-color:var(--crit)}
.empty--crit::before,.empty--crit::after{border-color:var(--crit)}
.empty__body{font-size:var(--sm);color:var(--ink-low);max-width:60ch;margin:0 auto;line-height:1.7}
.empty__body code{background:var(--bg-deep);padding:1px 6px;border:1px solid var(--rule);color:var(--ink-mid);font-size:11.5px}
.arch-list{display:flex;flex-wrap:wrap;gap:6px;margin-top:4px}
.arch{font-family:var(--font-mono);font-size:10.5px;letter-spacing:0.04em;padding:2px 8px;background:var(--bg-deep);color:var(--ink-mid);border:1px solid var(--rule);text-transform:lowercase}
.fp{display:inline-block;font-family:var(--font-mono);font-size:11.5px;letter-spacing:0.04em;color:var(--ink-high);font-variant-numeric:tabular-nums;word-spacing:2px;white-space:nowrap}
.fp--sub{color:var(--ink-low);font-size:10.5px}
.uid{display:block;font-size:12.5px;color:var(--ink-high);line-height:1.4}
.uid__email{display:block;font-family:var(--font-mono);font-size:10.5px;color:var(--ink-low);margin-top:2px}
.subkeys{display:flex;flex-direction:column;gap:3px}
.subkeys--none{color:var(--rule-hard);font-size:14px}
.src{display:inline-block;padding:1px 7px 2px;font-family:var(--font-mono);font-size:9.5px;letter-spacing:0.14em;font-weight:500;text-transform:uppercase;border:1px solid currentColor;border-radius:var(--r-badge);background:transparent;white-space:nowrap}
.src--bundled{color:var(--ink-low);border-color:var(--rule-hard)}
.src--system{color:var(--ink-mid);border-color:var(--rule-hard)}
.src--custom{color:var(--signal)}
.src-path{display:inline-block;margin-left:8px;font-family:var(--font-mono);font-size:11px;color:var(--ink-low)}
.col-hint{display:inline-block;margin-left:4px;cursor:help;position:relative}
.col-hint summary{list-style:none;display:inline-block;width:13px;height:13px;border-radius:50%;border:1px solid var(--rule-hard);color:var(--ink-low);font-size:9px;font-weight:600;line-height:11px;text-align:center;text-transform:none;letter-spacing:0;vertical-align:middle;cursor:help;transition:color 120ms ease,border-color 120ms ease}
.col-hint summary::-webkit-details-marker{display:none}
.col-hint:hover summary{color:var(--ink-high);border-color:var(--ink-low)}
.col-hint[open] summary{color:var(--signal);border-color:var(--signal)}
.col-hint__body{position:absolute;top:22px;left:-8px;z-index:5;background:var(--bg-panel);border:1px solid var(--rule-hard);padding:10px 14px;width:280px;font-size:12px;letter-spacing:0;text-transform:none;color:var(--ink-mid);line-height:1.55;font-weight:400;box-shadow:0 1px 0 0 var(--rule)}
footer{margin-top:80px;padding-top:20px;border-top:1px solid var(--rule);font-family:var(--font-mono);font-size:10px;letter-spacing:0.12em;text-transform:uppercase;color:var(--ink-low);display:flex;gap:14px;align-items:center;flex-wrap:wrap}
footer .sep{color:var(--rule)}
footer a{color:var(--signal);border-bottom:0;font-weight:600}
footer a:hover{color:var(--ink-high)}
.noscript-warn{margin:0 0 16px 0;padding:10px 14px;background:var(--bg-panel);border:1px solid var(--rule);font-size:var(--sm);color:var(--ink-mid)}
@media (max-width:1279px){
.vitals{grid-template-columns:repeat(3,minmax(0,1fr))}
.vital:nth-child(3){border-right:0}
.vital:nth-child(n+4){border-top:1px solid var(--rule)}
.vital:nth-child(4){border-right:1px solid var(--rule)}
.layout{grid-template-columns:1fr;gap:32px}
.rail{position:sticky;top:var(--bar-h);background:var(--bg);padding:12px 0;border-bottom:1px solid var(--rule)}
.rail__head{display:none}
.rail ul{flex-direction:row;flex-wrap:wrap;gap:0 14px}
.rail a{padding:4px 0}
.rail a.is-active::before,.rail a[aria-current="location"]::before{display:none}
.rail a.is-active,.rail a[aria-current="location"]{border-bottom:2px solid var(--signal) !important}
}
@media (max-width:720px){
body{background-image:none}
.page{padding:16px}
.bar{padding:8px 12px;gap:8px;flex-wrap:wrap;height:auto;min-height:var(--bar-h)}
.plate{margin:0;padding:6px 10px 6px 0;border-right:0}
.plate__sn{display:none}
.verdict{width:100%;order:3;padding:4px 0}
.meta{margin-left:auto;gap:8px;flex-wrap:wrap}
.vitals{grid-template-columns:1fr}
.vital{border-right:0;border-bottom:1px solid var(--rule)}
.vital:last-child{border-bottom:0}
.kv{grid-template-columns:1fr}
.kv > div{border-bottom:0;padding:6px 14px;border-right:0 !important;text-align:left !important}
table.data thead{display:none}
table.data tbody tr{display:block;border-bottom:1px solid var(--rule);padding:12px 14px}
table.data tbody tr[data-state] td:first-child{box-shadow:none}
table.data tbody tr[data-state="warn"]{border-left:3px solid var(--warn)}
table.data tbody tr[data-state="crit"]{border-left:3px solid var(--crit)}
table.data tbody td{display:flex;justify-content:space-between;padding:3px 0;white-space:normal;border-bottom:0}
table.data tbody td::before{content:attr(data-label);font-family:var(--font-mono);font-size:9.5px;letter-spacing:0.14em;text-transform:uppercase;color:var(--ink-low);margin-right:12px;flex-shrink:0}
}
@media (prefers-reduced-motion:reduce){*,*::before,*::after{animation-duration:0.01ms !important;transition-duration:0.01ms !important}}
</style>
</head>
<body data-uptime-seconds="{{.Process.UptimeSeconds}}" data-gc-runs="{{if and .GC .GC.LastRunUnixTime}}1{{else}}0{{end}}">

<svg width="0" height="0" style="position:absolute" aria-hidden="true"><defs>
<symbol id="i-sun" viewBox="0 0 16 16"><circle cx="8" cy="8" r="3" stroke="currentColor" stroke-width="1.4" fill="none"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3 3l1.4 1.4M11.6 11.6L13 13M3 13l1.4-1.4M11.6 4.4L13 3" stroke="currentColor" stroke-width="1.4"/></symbol>
<symbol id="i-moon" viewBox="0 0 16 16"><path d="M13 9.5A5 5 0 016.5 3 5 5 0 1013 9.5z" stroke="currentColor" stroke-width="1.4" fill="none"/></symbol>
<symbol id="i-arrow-out" viewBox="0 0 16 16"><path d="M6 4h6v6M11 5L4 12" stroke="currentColor" stroke-width="1.5" fill="none" stroke-linecap="square"/></symbol>
</defs></svg>

{{$kbundled := countBundled .Keyring}}{{$ksystem := countSystem .Keyring}}{{$kcustom := countCustom .Keyring}}{{$kcount := len .Keyring}}{{$adopting := .AdoptionEnabled}}{{$keyringCritState := and $adopting (eq $kcount 0) (not .AcceptAnySigner)}}

<header class="bar" role="banner">
  <div class="plate" aria-label="apt-cacher-ultra serial plate">
    <span class="plate__mark" aria-hidden="true">&laquo;</span>
    <span class="plate__name">apt-cacher-ultra</span>
    <span class="plate__sn">
      <span>SERIAL</span>
      <b>{{.Process.Version}}</b>
    </span>
  </div>

  <div class="verdict" role="status" aria-live="polite">
    <span class="verdict__pill" id="verdict-pill" data-state="stale">
      <span class="verdict__dot"></span>
      <span id="verdict-label">STATUS</span>
    </span>
    <span class="verdict__msg" id="verdict-msg">{{verdictExplanation .}}</span>
  </div>

  <div class="meta">
    <a href="#keyring" class="chip"
       data-keyring-count="{{$kcount}}"
       data-adoption-enabled="{{if $adopting}}true{{else}}false{{end}}"
       {{if $keyringCritState}}data-state="crit"{{end}}
       aria-label="GPG keys loaded; jump to Keyring section">
      <span>KEYS</span>
      <span class="chip__count">{{$kcount}}</span>
      <span class="chip__ok" aria-hidden="true">{{if $keyringCritState}}!{{else}}&#10003;{{end}}</span>
    </a>
    <span class="sep">&middot;</span>
    <span>BUILD <span class="meta__val" title="{{.Process.VCSRevision}}">{{if gt (len .Process.VCSRevision) 7}}{{slice .Process.VCSRevision 0 7}}{{else}}{{.Process.VCSRevision}}{{end}}</span></span>
    <span class="sep">&middot;</span>
    <button id="theme-toggle" class="icon-btn" type="button" aria-label="Toggle theme">
      <svg id="theme-icon"><use href="#i-sun"/></svg>
    </button>
    <a href="?format=json" class="icon-btn" aria-label="View as JSON">
      <svg><use href="#i-arrow-out"/></svg>
    </a>
  </div>
</header>

<main class="page">

  {{$cacheState := vitalState "cache" .}}{{$suitesState := vitalState "suites" .}}{{$adoptionsState := vitalState "adoptions" .}}{{$gcState := vitalState "gc" .}}{{$activeState := vitalState "active" .}}

  <noscript>
    <p class="noscript-warn">JavaScript is disabled &mdash; verdict pill and aggregate-failure notice will not update. Per-cell badges below carry the full state. Times stay in UTC.</p>
  </noscript>

  <section class="vitals" aria-label="Vital signs">
    <article class="vital" data-vital="cache" data-state="{{$cacheState}}">
      <header class="vital__head"><span class="vital__label">Cache</span></header>
      <div class="vital__readout">
        <span class="vital__num">{{formatBytes .Cache.BytesUsed}}</span>
      </div>
      <div class="vital__sub">
        <span>{{.Cache.BlobCount}} BLOBS &middot; {{.Cache.URLPathCount}} PATHS</span>
        {{if gt .Cache.ActuallyReapableBlobs 1000}}<span>BACKLOG <b class="accent-warn">{{.Cache.ActuallyReapableBlobs}} &uarr;</b></span>{{else}}<span>BACKLOG <b>{{.Cache.ActuallyReapableBlobs}}</b></span>{{end}}
      </div>
    </article>

    <article class="vital" data-vital="suites" data-state="{{$suitesState}}">
      <header class="vital__head"><span class="vital__label">Suites</span></header>
      <div class="vital__readout">
        <span class="vital__num">{{len .Suites}}</span>
        <span class="vital__unit">TRACKED</span>
      </div>
      <div class="vital__sub">
        {{$lagCount := 0}}{{range .Suites}}{{if .Lagging}}{{$lagCount = (add1 $lagCount)}}{{end}}{{end}}
        {{if eq $lagCount 0}}<span><b>0 LAGGING</b> &middot; 0 FAILING</span>{{else}}<span><b class="accent-warn">{{$lagCount}} LAGGING</b> &middot; 0 FAILING</span>{{end}}
      </div>
    </article>

    <article class="vital" data-vital="adoptions" data-state="{{$adoptionsState}}">
      <header class="vital__head"><span class="vital__label">Adoptions</span></header>
      {{$nA := len .RecentAdoptions}}{{$okA := 0}}{{$failA := 0}}{{range .RecentAdoptions}}{{if eq .Outcome "success"}}{{$okA = (add1 $okA)}}{{else}}{{$failA = (add1 $failA)}}{{end}}{{end}}
      <div class="vital__readout">
        {{if eq $nA 0}}<span class="vital__num muted">&mdash;</span><span class="vital__unit">NO EVENTS</span>{{else}}<span class="vital__num">{{$okA}}</span><span class="vital__unit">/ {{$nA}} OK</span>{{end}}
      </div>
      <div class="vital__sub">
        {{if eq $nA 0}}<span>RING EMPTY</span>{{else if eq $failA 0}}<span><b class="accent-ok">RING CLEAN</b></span>{{else}}<span><b class="accent-crit">{{$failA}} FAILED</b></span>{{end}}
        <span>{{$nA}} EVENTS IN RING</span>
      </div>
    </article>

    <article class="vital" data-vital="gc" data-state="{{$gcState}}">
      <header class="vital__head"><span class="vital__label">Last GC</span></header>
      {{if and .GC .GC.LastRunUnixTime}}
        <div class="vital__readout">
          <span class="vital__num">{{formatShortDuration .GC.LastRunDurationSeconds}}</span>
        </div>
        <div class="vital__sub">
          <span>{{unixTimePtr .GC.LastRunUnixTime}} &middot; {{defaultEmpty .GC.LastRunPhase "(unknown)"}}</span>
          <span>RECLAIMED <b>{{formatBytes .GC.LastRunBytesReclaimed}}</b> &middot; {{.GC.LastRunBlobsReaped}} BLOBS</span>
        </div>
      {{else}}
        <div class="vital__readout">
          <span class="vital__num muted">&mdash;</span>
        </div>
        <div class="vital__sub"><span>NO GC RUN YET</span><span>WARMING UP</span></div>
      {{end}}
    </article>

    <article class="vital" data-vital="active" data-state="{{$activeState}}">
      <header class="vital__head"><span class="vital__label">Active fetch</span></header>
      <div class="vital__readout">
        <span class="vital__num">{{len .ActiveHosts}}</span>
        <span class="vital__unit">HOSTS</span>
      </div>
      <div class="vital__sub">
        {{if .ActiveHosts}}<span>CURRENTLY FETCHING</span>{{else}}<span>IDLE &middot; AWAITING TRAFFIC</span>{{end}}
        <span>{{if .ActiveHosts}}SEE PANEL BELOW{{else}}NO FETCHES IN FLIGHT{{end}}</span>
      </div>
    </article>
  </section>

  <div class="layout">
    <nav class="rail" aria-label="Section navigation">
      <div class="rail__head"><span>Detail</span><span>9 panels</span></div>
      <ul>
        <li><a href="#suites">Suites</a></li>
        <li><a href="#adoptions">Adoptions</a></li>
        <li><a href="#keyring">Keyring</a></li>
        <li><a href="#hot">Hot paths</a></li>
        <li><a href="#by-host">Host &times; arch</a></li>
        <li><a href="#coverage">Coverage</a></li>
        <li><a href="#gc">GC</a></li>
        <li><a href="#active">Active hosts</a></li>
        <li><a href="#plumbing">Plumbing</a></li>
      </ul>
    </nav>

    <div class="content">

      <!-- SUITES -->
      <section class="panel" id="suites">
        <div class="panel__eyebrow">
          {{$lc := 0}}{{range .Suites}}{{if .Lagging}}{{$lc = (add1 $lc)}}{{end}}{{end}}
          <span>Suites</span><span class="sep">&mdash;</span>
          <span>{{len .Suites}} TRACKED</span>
          {{if gt $lc 0}}<span class="sep">&middot;</span><span class="count-warn">{{$lc}} LAGGING</span>{{end}}
        </div>
        <h2 class="panel__h">Suite adoption status</h2>
        <p class="panel__desc">One row per (host, suite). Lagging rows indicate the upstream has published a newer <code>InRelease</code> than the snapshot the cache is currently serving.</p>
        {{if .Suites}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr>
              <th>Host</th><th>Suite path</th><th>Last check</th><th>Last success</th>
              <th class="num">Snapshot</th>
              <th>Adopted<details class="col-hint"><summary aria-label="What does Adopted mean?">i</summary><div class="col-hint__body">Adoption fires only when a fresh InRelease has been observed. Suites whose upstream has not republished since process start may stay empty here without being broken.</div></details></th>
              <th>InRelease changed</th>
            </tr></thead>
            <tbody>
            {{range .Suites}}
              <tr{{if .Lagging}} data-state="warn"{{end}}>
                <td data-label="Host" class="host">{{.Host}}</td>
                <td data-label="Suite path" class="mono">{{.SuitePath}}</td>
                <td data-label="Last check" class="time">{{unixTimePtr .LastCheckUnixTime}}</td>
                <td data-label="Last success" class="time">{{unixTimePtr .LastSuccessUnixTime}}</td>
                <td data-label="Snapshot" class="num mono">{{i64Ptr .CurrentSnapshotID}}</td>
                <td data-label="Adopted" class="time">{{unixTimePtr .CurrentSnapshotAdoptedAtUnixTime}}</td>
                <td data-label="InRelease changed" class="time">{{unixTimePtr .InReleaseChangeSeenAtUnixTime}}{{if .Lagging}} <span class="lagging">{{.Lagging}}</span>{{end}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">NO SUITES TRACKED YET</div><div class="empty__body">Suites populate after the first adoption cycle.</div></div>{{end}}
      </section>

      <!-- RECENT ADOPTIONS -->
      <section class="panel" id="adoptions">
        {{$na := len .RecentAdoptions}}{{$okN := 0}}{{$failN := 0}}{{range .RecentAdoptions}}{{if eq .Outcome "success"}}{{$okN = (add1 $okN)}}{{else}}{{$failN = (add1 $failN)}}{{end}}{{end}}
        <div class="panel__eyebrow">
          <span>Adoptions</span><span class="sep">&mdash;</span>
          <span>{{$na}} IN RING</span>
          {{if gt $failN 0}}<span class="sep">&middot;</span><span class="count-crit">{{$failN}} FAILED</span>{{end}}
          {{if gt $okN 0}}<span class="sep">&middot;</span><span class="count-ok">{{$okN}} SUCCESS</span>{{end}}
        </div>
        <h2 class="panel__h">Recent adoption outcomes</h2>

        <div id="adoptions-notice" class="notice-mount" data-notice-total="{{$na}}"></div>

        {{if .RecentAdoptions}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr>
              <th>Host</th><th>Suite path</th><th>Outcome</th><th>Completed</th><th class="num">Duration</th>
            </tr></thead>
            <tbody>
            {{range .RecentAdoptions}}
              <tr{{if ne .Outcome "success"}} data-state="crit"{{end}} data-outcome="{{.Outcome}}"{{if .Reason}} data-reason="{{.Reason}}"{{end}}>
                <td data-label="Host" class="host">{{.Host}}</td>
                <td data-label="Suite path" class="mono">{{.SuitePath}}</td>
                <td data-label="Outcome"><span class="b {{outcomeBadgeClass .Outcome}}">{{.Outcome}}</span>{{if and .Reason (ne .Reason .Outcome)}} <span class="b b--neutral reason" title="{{reasonTooltip .Reason}}">{{.Reason}}</span>{{end}}</td>
                <td data-label="Completed" class="time">{{unixTime .CompletedUnixTime}}</td>
                <td data-label="Duration" class="num mono">{{formatShortDuration .DurationSeconds}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else if lt .Process.UptimeSeconds 300}}<div class="empty"><div class="empty__head">NO ADOPTIONS YET</div><div class="empty__body">Empty since this process started.</div></div>{{end}}
      </section>

      <!-- KEYRING -->
      <section class="panel" id="keyring" data-accept-any-signer="{{if .AcceptAnySigner}}true{{else}}false{{end}}">
        <div class="panel__eyebrow">
          <span>Keyring</span><span class="sep">&mdash;</span>
          <span data-keyring-count="{{$kcount}}"><b>{{$kcount}}</b> LOADED</span>
          <span class="sep">&middot;</span><span data-keyring-bundled="{{$kbundled}}">{{$kbundled}} BUNDLED</span>
          <span class="sep">&middot;</span><span data-keyring-system="{{$ksystem}}">{{$ksystem}} SYSTEM</span>
          <span class="sep">&middot;</span><span data-keyring-custom="{{$kcustom}}">{{$kcustom}} CUSTOM</span>
          {{if .AcceptAnySigner}}<span class="sep">&middot;</span><span class="b b--warn" title="adoption.accept_any_signer is true: unpinned suites bypass signature verification at adoption time. Apt clients on the fleet remain the authoritative trust anchor.">Trust: accept any signer</span>{{end}}
        </div>
        <h2 class="panel__h">Trusted GPG keys</h2>
        <p class="panel__desc">Keys used to verify upstream <code>InRelease</code> signatures during adoption. Bundled keys ship with the binary; custom keys come from <code>keyring_dirs</code> paths.{{if .AcceptAnySigner}} With <code>accept_any_signer = true</code>, unpinned suites are adopted without consulting these keys; pinned suites still require a match.{{end}}</p>
        {{if .Keyring}}
        <div class="table-wrap">
          <table class="data" id="keyring-table">
            <thead><tr><th>Primary fingerprint</th><th>User ID</th><th>Source</th><th>Subkey fingerprints</th></tr></thead>
            <tbody>
            {{range .Keyring}}{{$kind := sourceKind .SourcePath}}{{$uid := splitUID .PrimaryUID}}
              <tr data-source-kind="{{$kind}}">
                <td data-label="Primary fingerprint"><span class="fp">{{chunkHex .PrimaryFingerprint 4}}</span></td>
                <td data-label="User ID"><span class="uid" title="{{.PrimaryUID}}">{{$uid.Name}}{{if $uid.Email}}<span class="uid__email">{{$uid.Email}}</span>{{end}}</span></td>
                <td data-label="Source"><span class="src src--{{$kind}}">{{sourceKindLabel .SourcePath}}</span><span class="src-path">{{.SourcePath}}</span></td>
                <td data-label="Subkey fingerprints">{{if .SubkeyFingerprints}}<div class="subkeys">{{range .SubkeyFingerprints}}<span class="fp fp--sub">{{chunkHex . 4}}</span>{{end}}</div>{{else}}<span class="subkeys subkeys--none">&mdash;</span>{{end}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else if and $adopting .AcceptAnySigner}}<div class="empty"><div class="empty__head">NO GPG KEYS LOADED</div><div class="empty__body">Adoption is enabled with <code>accept_any_signer = true</code>; unpinned suites bypass signature verification, so an empty keyring is workable here. Apt clients on the fleet remain the authoritative trust anchor.</div></div>
        {{else if $adopting}}<div class="empty empty--crit"><div class="empty__head">NO GPG KEYS LOADED</div><div class="empty__body">Adoption is enabled but the keyring is empty. All <code>InRelease</code> verifications will fail. Check <code>keyring_dirs</code> in the configuration.</div></div>
        {{else}}<div class="empty"><div class="empty__head">ADOPTION DISABLED</div><div class="empty__body">No GPG keys are loaded because adoption is disabled in the configuration.</div></div>{{end}}
      </section>

      <!-- HOT URL PATHS -->
      <section class="panel" id="hot">
        <div class="panel__eyebrow"><span>Hot URL paths</span><span class="sep">&mdash;</span><span>TOP {{len .HotURLPaths}} BY REQUEST COUNT</span></div>
        <h2 class="panel__h">What clients are asking for</h2>
        {{if .HotURLPaths}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr><th>Host</th><th>Path</th><th>Kind</th><th class="num">Requests</th><th>Last requested</th></tr></thead>
            <tbody>
            {{range .HotURLPaths}}
              <tr>
                <td data-label="Host" class="host">{{.Host}}</td>
                <td data-label="Path" class="mono">{{.Path}}</td>
                <td data-label="Kind"><span class="b {{if .IsMetadata}}b--neutral{{else}}b--ok{{end}}">{{if .IsMetadata}}metadata{{else}}payload{{end}}</span></td>
                <td data-label="Requests" class="num mono">{{.RequestCount}}</td>
                <td data-label="Last requested" class="time">{{unixTime .LastRequestedUnixTime}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">NO URL PATHS YET</div><div class="empty__body">The hot-paths table populates after the first cached request.</div></div>{{end}}
      </section>

      <!-- BY-HOST -->
      <section class="panel" id="by-host">
        <div class="panel__eyebrow"><span>Cache contents</span><span class="sep">&mdash;</span><span>BY HOST &times; ARCHITECTURE</span></div>
        <h2 class="panel__h">What is on disk, broken down</h2>
        {{with .CacheSummary.Sorted}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr><th>Host</th><th>Architecture</th><th class="num">package_hash rows</th><th class="num">Blobs</th><th class="num">Bytes</th></tr></thead>
            <tbody>
            {{range .}}{{$host := .Host}}{{range .Architectures}}
              <tr>
                <td data-label="Host" class="host">{{$host}}</td>
                <td data-label="Architecture"><span class="arch">{{.Arch}}</span></td>
                <td data-label="package_hash rows" class="num mono">{{.Entry.PackageHashCount}}</td>
                <td data-label="Blobs" class="num mono">{{.Entry.BlobCount}}</td>
                <td data-label="Bytes" class="num mono">{{formatBytes .Entry.BlobBytes}}</td>
              </tr>
            {{end}}{{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">NO CACHED BLOBS YET</div><div class="empty__body">The by-host breakdown populates after the first adoption cycle.</div></div>{{end}}
      </section>

      <!-- COVERAGE -->
      <section class="panel" id="coverage">
        <div class="panel__eyebrow"><span>Repository coverage</span></div>
        <h2 class="panel__h">What the cache is covering</h2>
        <div class="kv">
          <div class="k">Architectures seen</div>
          <div class="v">{{if .RepoCoverage.ArchitecturesSeen}}<div class="arch-list">{{range .RepoCoverage.ArchitecturesSeen}}<span class="arch">{{.}}</span>{{end}}</div>{{else}}<span class="muted">(none &mdash; no current snapshots have package_hash rows yet)</span>{{end}}</div>
          <div class="k">Architectures filter</div>
          <div class="v">{{if .RepoCoverage.ArchitecturesFilter}}<div class="arch-list">{{range .RepoCoverage.ArchitecturesFilter}}<span class="arch">{{.}}</span>{{end}}</div>{{else}}<code>(unfiltered &mdash; all Release-listed indices adopted)</code>{{end}}</div>
          <div class="k">Snapshots with Sources</div>
          <div class="v"><code>{{.RepoCoverage.SnapshotsWithSources}}</code></div>
          <div class="k">Snapshots with pdiff</div>
          <div class="v"><code>{{.RepoCoverage.SnapshotsWithPdiff}}</code></div>
          <div class="k">package_hash rows (binary)</div>
          <div class="v"><code>{{.RepoCoverage.PackageHashRows.Binary}}</code></div>
          <div class="k">package_hash rows (source)</div>
          <div class="v"><code>{{.RepoCoverage.PackageHashRows.Source}}</code></div>
          <div class="k">package_hash rows (pdiff)</div>
          <div class="v"><code>{{.RepoCoverage.PackageHashRows.Pdiff}}</code></div>
          <div class="k">package_hash rows (total)</div>
          <div class="v"><code>{{.RepoCoverage.PackageHashRows.Total}}</code></div>
        </div>
      </section>

      <!-- GC -->
      <section class="panel" id="gc">
        <div class="panel__eyebrow">
          <span>Garbage collection</span>
          {{if and .GC .GC.LastRunUnixTime}}<span class="sep">&mdash;</span><span>LAST RUN {{unixTimePtr .GC.LastRunUnixTime}}</span>{{end}}
          <span class="sep">&middot;</span><span class="count-{{$gcState}}">{{$gcState}}</span>
        </div>
        <h2 class="panel__h">Cache reaping</h2>
        {{if and .GC .GC.LastRunUnixTime}}
        <div class="kv">
          <div class="k">Last run</div>
          <div class="v"><span class="time mono">{{unixTimePtr .GC.LastRunUnixTime}}</span> &middot; <span class="b b--neutral">{{defaultEmpty .GC.LastRunPhase "unknown"}}</span></div>
          <div class="k">Duration</div>
          <div class="v"><code>{{formatShortDuration .GC.LastRunDurationSeconds}}</code></div>
          <div class="k">Blobs reaped</div>
          <div class="v"><code>{{.GC.LastRunBlobsReaped}}</code></div>
          <div class="k">Bytes reclaimed</div>
          <div class="v"><code>{{formatBytes .GC.LastRunBytesReclaimed}}</code></div>
          <div class="k">Orphan candidates reaped</div>
          <div class="v"><code>{{.GC.OrphanCandidatesReaped}}</code></div>
          <div class="k">Displaced reaped</div>
          <div class="v"><code>{{.GC.DisplacedReaped}}</code></div>
          <div class="k">Pool orphans repaired</div>
          <div class="v"><code>{{.GC.PoolOrphansRepaired}}</code> <span class="muted">({{formatBytes .GC.PoolOrphanBytesRepaired}})</span></div>
          <div class="k">Pool unlink errors</div>
          <div class="v"><code>{{.GC.PoolUnlinkErrors}}</code></div>
          <div class="k">Deadline reached</div>
          <div class="v"><span class="b {{if .GC.LastRunDeadlineReached}}b--crit{{else}}b--ok{{end}}">{{.GC.LastRunDeadlineReached}}</span></div>
        </div>
        {{else}}<div class="empty"><div class="empty__head">NO GC RUN YET</div><div class="empty__body">GC has not completed since process start.</div></div>{{end}}
      </section>

      <!-- ACTIVE HOSTS -->
      <section class="panel" id="active">
        <div class="panel__eyebrow"><span>Active hosts</span><span class="sep">&mdash;</span><span>FETCH-SLOT SEMAPHORE SNAPSHOT</span></div>
        <h2 class="panel__h">Upstream fetches in flight</h2>
        {{if .ActiveHosts}}
        <div class="table-wrap">
          <table class="data">
            <thead><tr><th>Host</th><th class="num">Inflight</th><th class="num">Slot capacity</th></tr></thead>
            <tbody>
            {{range .ActiveHosts}}
              <tr>
                <td data-label="Host" class="host">{{.Host}}</td>
                <td data-label="Inflight" class="num mono">{{.Inflight}}</td>
                <td data-label="Slot capacity" class="num mono">{{.SlotCapacity}}</td>
              </tr>
            {{end}}
            </tbody>
          </table>
        </div>
        {{else}}<div class="empty"><div class="empty__head">CHANNEL IDLE &mdash; NO FETCHES</div><div class="empty__body">No hosts hold a fetch slot at this instant. Slot usage is bursty; this is the steady-state reading between adoption cycles.</div></div>{{end}}
      </section>

      <!-- PLUMBING -->
      <section class="panel" id="plumbing">
        <div class="panel__eyebrow"><span>Plumbing</span><span class="sep">&mdash;</span><span>LISTENERS &middot; TLS MITM &middot; PROCESS</span></div>
        <h2 class="panel__h">Bindings, certificates, build</h2>

        <div class="kv" style="margin-bottom:24px">
          <div class="kv__group-head"><span>Listeners</span></div>
          {{range .Listeners}}<div class="k">{{.Role}}</div><div class="v"><code>{{.Addr}}</code></div>{{end}}
        </div>

        {{if .TLSMITM.Enabled}}
        <div class="kv" style="margin-bottom:24px">
          <div class="kv__group-head"><span>TLS MITM</span> <span class="sep">&mdash;</span> <span class="b b--ok">enabled</span></div>
          <div class="k">CA source</div>
          <div class="v"><span class="b b--neutral">{{.TLSMITM.CASource}}</span></div>
          <div class="k">CA fingerprint (SHA-256)</div>
          <div class="v"><span class="fp">{{chunkHex .TLSMITM.CAFingerprintSHA256 4}}</span></div>
          <div class="k">CA not_after</div>
          <div class="v"><span class="time mono">{{unixTime .TLSMITM.CANotAfterUnixTime}}</span></div>
          <div class="k">Effective allowlist</div>
          <div class="v"><code>{{defaultEmpty .TLSMITM.EffectiveAllowlist "(none — vacuously true)"}}</code></div>
          <div class="k">Cert cache</div>
          <div class="v"><code>{{.TLSMITM.CertCache.Size}} / {{.TLSMITM.CertCache.Capacity}}</code></div>
          <div class="k">Last cert issued</div>
          <div class="v">{{if .TLSMITM.LastIssued}}<code>{{.TLSMITM.LastIssued.Host}}</code> @ <span class="time mono">{{unixTime .TLSMITM.LastIssued.AtUnixTime}}</span>{{else}}<span class="muted">(none yet)</span>{{end}}</div>
          <div class="k">Cert hit rate (60s)</div>
          <div class="v"><code>{{hitRatePct .TLSMITM.HitRate60sPercent .TLSMITM.HitRate60sObserved}}</code></div>
        </div>
        {{end}}

        <div class="kv">
          <div class="kv__group-head"><span>Process</span></div>
          <div class="k">Version</div>
          <div class="v"><code>{{.Process.Version}}</code></div>
          <div class="k">Started</div>
          <div class="v"><span class="time mono">{{unixTime .Process.StartedUnixTime}}</span> &middot; uptime <span class="mono" style="color:var(--ink-mid)">{{durationOf .Process.UptimeSeconds}}</span></div>
          <div class="k">Build</div>
          <div class="v"><code>{{.Process.VCSRevision}}</code></div>
          <div class="k">Go version</div>
          <div class="v"><code>{{.Process.GoVersion}}</code></div>
          <div class="k">Cache directory</div>
          <div class="v"><code>{{.Cache.Dir}}</code></div>
        </div>
      </section>

    </div>
  </div>

  <footer>
    <a href="/metrics">/metrics</a>
    <span class="sep">&middot;</span>
    <a href="/healthz">/healthz</a>
    <span class="sep">&middot;</span>
    <span>Times in UTC, rewritten to browser-local on load</span>
    <span class="sep">&middot;</span>
    <span>Page auto-refresh 60 s</span>
    <span class="sep">&middot;</span>
    <span>BUILD <span class="meta__val" title="{{.Process.VCSRevision}}">{{if gt (len .Process.VCSRevision) 7}}{{slice .Process.VCSRevision 0 7}}{{else}}{{.Process.VCSRevision}}{{end}}</span></span>
  </footer>
</main>

<script>
(function(){var r=document.documentElement,b=document.getElementById('theme-toggle'),i=document.getElementById('theme-icon');if(!b||!i)return;
function paint(){var m=r.getAttribute('data-theme');if(!m||m==='auto'){m=window.matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light';}i.firstElementChild.setAttribute('href',m==='dark'?'#i-sun':'#i-moon');}
paint();
b.addEventListener('click',function(){var c=r.getAttribute('data-theme');if(!c||c==='auto'){c=window.matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light';}var n=c==='dark'?'light':'dark';r.setAttribute('data-theme',n);try{localStorage.setItem('acu-theme',n);}catch(e){}paint();});})();

(function(){var pill=document.getElementById('verdict-pill'),lbl=document.getElementById('verdict-label');if(!pill||!lbl)return;
var states=[].slice.call(document.querySelectorAll('[data-state]')).map(function(el){return el.getAttribute('data-state');});
var keys=document.querySelector('.chip');var keyringCrit=keys&&keys.getAttribute('data-state')==='crit';
var verdict='ok',cls='ok',label='HEALTHY';
if(states.indexOf('crit')!==-1||keyringCrit){verdict='crit';label='DEGRADED';}
else if(states.indexOf('warn')!==-1){verdict='warn';label='WATCHING';}
else{var b=document.body,up=parseInt(b.getAttribute('data-uptime-seconds')||'0',10),gr=parseInt(b.getAttribute('data-gc-runs')||'0',10);if(up<300&&gr===0){verdict='stale';label='WARMING UP';}}
pill.setAttribute('data-state',verdict);lbl.textContent=label;})();

(function(){var mount=document.getElementById('adoptions-notice');if(!mount)return;
var rows=document.querySelectorAll('#adoptions tr[data-outcome]');var total=rows.length;if(total===0)return;
var counts={};rows.forEach(function(tr){var o=tr.getAttribute('data-outcome');if(o==='success'||!o)return;counts[o]=(counts[o]||0)+1;});
var top='',topN=0;for(var k in counts){if(counts[k]>topN){top=k;topN=counts[k];}}
var THRESHOLD=0.10;if(top===''||(topN/total)<THRESHOLD)return;
var keyringPanel=document.getElementById('keyring');
var acceptAnySigner=keyringPanel&&keyringPanel.getAttribute('data-accept-any-signer')==='true';
var gpgFailedText=acceptAnySigner
?"Likely cause: with accept_any_signer = true, gpg_failed typically indicates a structural decode failure (corrupt clearsign envelope or Release.gpg) or a pinned-suite trust mismatch. Cross-check the Keyring section."
:"Likely cause: upstream repository key changed or the matching archive key isn't loaded. Cross-check the Keyring section.";
var hints={
'gpg_failed':{text:gpgFailedText,linkHref:'#keyring',linkLabel:'Trusted keys',linkArrow:String.fromCharCode(8594)+' Keyring'},
'parse_failed':{text:'Likely cause: malformed Release / Sources / Packages payload from upstream. Capture a failing fetch and inspect.'},
'member_mismatch':{text:'Likely cause: a Release-listed member hash diverged from the cached blob. Inspect the failing index path in the proxy logs.'},
'unpinned_suite':{text:"Likely cause: this suite is not allow-listed for adoption. Add it to the operator's adoption pin list to enable verification."},
'run_failed':{text:'Likely cause: upstream unreachable, TLS failure, rate-limiting, or another transport-level error. Check the proxy logs for the upstream host.'}
};
var h=hints[top]||{text:'See per-row details below.'};
var note=document.createElement('div');note.className='notice';note.setAttribute('role','alert');
var head=document.createElement('div');head.className='notice__head';
head.appendChild(document.createTextNode(String.fromCharCode(9888)+' '));
var headText=document.createElement('span');headText.appendChild(document.createTextNode(topN+' of '+total+' recent adoptions failed: '));
var headCode=document.createElement('code');headCode.style.cssText='background:transparent;padding:0;color:var(--crit);border:0';headCode.textContent=top;headText.appendChild(headCode);
head.appendChild(headText);note.appendChild(head);
var body=document.createElement('div');body.className='notice__body';body.appendChild(document.createTextNode(h.text));
note.appendChild(body);
if(h.linkHref){var row=document.createElement('div');row.className='notice__link';row.appendChild(document.createTextNode(h.linkLabel+' '));var a=document.createElement('a');a.href=h.linkHref;a.textContent=h.linkArrow;row.appendChild(a);note.appendChild(row);}
mount.appendChild(note);})();

(function(){var rail=document.querySelector('.rail');if(!rail||!('IntersectionObserver' in window))return;
var links={};rail.querySelectorAll('a[href^="#"]').forEach(function(a){links[a.getAttribute('href').slice(1)]=a;});
var io=new IntersectionObserver(function(es){es.forEach(function(e){var id=e.target.id;if(!links[id])return;if(e.isIntersecting){Object.keys(links).forEach(function(k){links[k].removeAttribute('aria-current');});links[id].setAttribute('aria-current','location');}});},{rootMargin:'-72px 0px -65% 0px'});
document.querySelectorAll('section.panel').forEach(function(s){io.observe(s);});})();

(function(){document.addEventListener('click',function(e){document.querySelectorAll('details.col-hint[open]').forEach(function(d){if(!d.contains(e.target))d.open=false;});});document.addEventListener('keydown',function(e){if(e.key==='Escape')document.querySelectorAll('details.col-hint[open]').forEach(function(d){d.open=false;});});})();

(function(){var tz;try{tz=Intl.DateTimeFormat().resolvedOptions().timeZone;}catch(e){}if(!tz)return;
var pad=function(n){return n<10?'0'+n:''+n;};var tzS='';try{var p=new Intl.DateTimeFormat(undefined,{timeZoneName:'short'}).formatToParts(new Date());for(var i=0;i<p.length;i++){if(p[i].type==='timeZoneName'){tzS=p[i].value;break;}}}catch(e){}
var ns=document.querySelectorAll('time[data-unix]');for(var i=0;i<ns.length;i++){var u=parseInt(ns[i].getAttribute('data-unix'),10);if(!isFinite(u))continue;var d=new Date(u*1000);var s=d.getFullYear()+'-'+pad(d.getMonth()+1)+'-'+pad(d.getDate())+' '+pad(d.getHours())+':'+pad(d.getMinutes())+':'+pad(d.getSeconds());ns[i].textContent=tzS?(s+' '+tzS):s;}})();
</script>
</body>
</html>
`
