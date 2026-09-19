package admin

var injectPageHTML = []byte(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>Turn State Inject</title>
<link id="siteFavicon" rel="icon" href="/favicon.png"/>
<link id="siteTouchIcon" rel="apple-touch-icon" href="/favicon.png"/>
<style>
:root {
  --bg: hsl(220 24% 96%); --fg: hsl(222 40% 11%); --card: #fff;
  --border: hsl(214 18% 88%); --muted: hsl(220 10% 46%);
  --primary: hsl(214 84% 46%); --ok: hsl(152 50% 34%);
  --warn: hsl(36 72% 40%); --bad: hsl(0 60% 48%); --track: hsl(214 22% 92%);
  --soft: hsl(214 22% 94%);
}
.dark { --bg: hsl(222 16% 12%); --fg: hsl(215 16% 84%); --card: hsl(222 14% 17%);
  --border: hsl(222 10% 28%); --muted: hsl(216 9% 60%); --primary: hsl(205 62% 54%);
  --track: hsl(222 11% 26%); --soft: hsl(222 11% 22%); }
* { box-sizing: border-box; }
body { margin: 0; font-family: Inter, "Noto Sans SC", -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif; background: var(--bg); color: var(--fg); font-size: 14px; }
header.page { max-width: 1880px; margin: 0 auto; padding: 20px 24px 4px; display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; flex-wrap: wrap; }
h1 { font-size: 20px; margin: 0; letter-spacing: .2px; }
.brand-title { display: flex; align-items: center; gap: 10px; }
.site-logo { width: 34px; height: 34px; flex: none; object-fit: contain; border-radius: 9px; }
h2 { font-size: 13px; margin: 0 0 12px; color: var(--muted); font-weight: 600; letter-spacing: .4px; text-transform: uppercase; }
.sub { color: var(--muted); font-size: 12px; margin-top: 3px; }
main { max-width: 1880px; margin: 0 auto; padding: 14px 24px 40px; display: grid; gap: 14px; }
.card { background: var(--card); border: 1px solid var(--border); border-radius: 14px; padding: 16px 18px; box-shadow: 0 1px 2px hsl(222 40% 11% / .04); }
.grid { display: grid; gap: 14px; }
.grid.two { grid-template-columns: repeat(auto-fit, minmax(320px, 1fr)); }
.settings-pair { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 14px; }
.settings-pair > .card { min-width: 0; }
.settings-form { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px 14px; align-items: end; }
.settings-form label { min-width: 0; }
.settings-form input, .settings-form select { width: 100%; min-width: 0; }
.settings-form .full-width { grid-column: 1 / -1; }
.settings-form .switchline { align-self: start; }
.settings-form > button { justify-self: start; }
.settings-pair .sub { line-height: 1.6; overflow-wrap: anywhere; }
.settings-actions { display: flex; justify-content: space-between; align-items: center; gap: 14px; padding: 12px 16px; border: 1px solid var(--border); border-radius: 12px; background: var(--card); }
.settings-actions .sub { margin: 3px 0 0; }
.settings-actions strong { font-size: 13px; }
.settings-actions .savebtn { flex-shrink: 0; padding: 9px 18px; }
@media (max-width: 960px) { .settings-pair { grid-template-columns: minmax(0, 1fr); } }
@media (max-width: 480px) {
  .settings-form { grid-template-columns: minmax(0, 1fr); }
  .settings-actions { align-items: stretch; flex-direction: column; }
}
.row { display: flex; flex-wrap: wrap; gap: 10px 14px; align-items: flex-end; }
label { font-size: 12px; color: var(--muted); display: flex; flex-direction: column; gap: 5px; font-weight: 500; }
input, button, select { font: inherit; }
select { border: 1px solid var(--border); border-radius: 9px; padding: 7px 10px; background: var(--card); color: var(--fg); min-width: 180px; max-width: 100%; }
.policy-status { margin: 0 12px 10px; padding: 7px 9px; border-radius: 8px; background: var(--soft); overflow-wrap: anywhere; font-size: 11.5px; color: var(--muted); }
input[type=text], input[type=password], input[type=number] {
  border: 1px solid var(--border); border-radius: 9px; padding: 7px 10px; background: transparent; color: inherit; min-width: 90px; outline: none; transition: border-color .15s, box-shadow .15s;
}
input:focus { border-color: var(--primary); box-shadow: 0 0 0 3px hsl(214 84% 46% / .15); }
button { border: 0; border-radius: 9px; padding: 7px 12px; background: var(--primary); color: #fff; cursor: pointer; font-size: 12.5px; font-weight: 600; transition: filter .15s, background .15s, transform .05s; }
button:hover { filter: brightness(1.06); }
button:active { transform: scale(.97); }
button:disabled { opacity: .45; cursor: default; transform: none; filter: none; }
button.ghost { background: transparent; color: var(--fg); border: 1px solid var(--border); }
button.ghost:hover { background: var(--soft); filter: none; }
button.danger { background: transparent; color: var(--bad); border: 1px solid var(--border); }
button.danger:hover { background: hsl(0 60% 48% / .08); filter: none; }
.switch { position: relative; width: 42px; height: 24px; border-radius: 99px; background: var(--track); border: 1px solid var(--border); cursor: pointer; transition: background .2s; flex: none; padding: 0; }
.switch::after { content: ''; position: absolute; top: 2px; left: 2px; width: 18px; height: 18px; border-radius: 50%; background: #fff; box-shadow: 0 1px 2px hsl(222 40% 11% / .25); transition: transform .2s; }
.switch.on { background: var(--ok); border-color: var(--ok); }
.switch.on::after { transform: translateX(18px); }
.switchline { display: flex; align-items: center; gap: 9px; font-size: 13px; font-weight: 600; }
.switchline .hint { font-weight: 400; font-size: 12px; color: var(--muted); }
.stat { display: flex; flex-direction: column; gap: 2px; padding: 7px 12px; border: 1px solid var(--border); border-radius: 10px; background: var(--bg); min-width: 92px; }
.stat b { font-size: 18px; font-variant-numeric: tabular-nums; }
.stat span { font-size: 11.5px; color: var(--muted); }
.pool-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.pool-head h2 { margin: 0; color: var(--fg); font-size: 16px; text-transform: none; }
.pool-head .row { align-items: center; gap: 7px; }
.pool-progress { display: flex; flex-direction: column; gap: 6px; margin-top: 14px; }
.pool-progress-label { display: flex; justify-content: space-between; gap: 12px; font-size: 12px; color: var(--muted); font-variant-numeric: tabular-nums; }
.pool-progress-track { height: 10px; background: var(--track); border-radius: 99px; overflow: hidden; }
.pool-progress-fill { display: block; height: 100%; width: 0; border-radius: 99px; background: var(--ok); transition: width .35s; }
.pool-progress-fill.task { background: var(--primary); }
.pool-progress-fill.paused { background: var(--warn); }
.account-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 480px), 1fr)); align-items: start; gap: 12px; }
.account-grid[data-columns="1"] { grid-template-columns: minmax(0, 1fr); }
.account-grid[data-columns="2"] { grid-template-columns: repeat(2, minmax(0, 1fr)); }
.account-grid[data-columns="3"] { grid-template-columns: repeat(3, minmax(0, 1fr)); }
.account-grid[data-columns="4"] { grid-template-columns: repeat(4, minmax(0, 1fr)); }
.pool-tools { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin: 12px 0 0; }
.segment { display: inline-flex; padding: 2px; gap: 2px; background: var(--soft); border: 1px solid var(--border); border-radius: 9px; }
.segment button { padding: 5px 9px; background: transparent; color: var(--muted); border-radius: 7px; font-size: 11.5px; }
.segment button[aria-pressed="true"] { background: var(--card); color: var(--fg); box-shadow: 0 1px 3px hsl(222 40% 11% / .15); }
.pool-tools .hint { color: var(--muted); font-size: 12px; }
.selection-actions { display: flex; gap: 6px; flex-wrap: wrap; align-items: center; margin-left: auto; }
.selection-actions button { padding: 5px 9px; }
.select-account { width: 16px; height: 16px; margin: 0; accent-color: var(--primary); cursor: pointer; flex: none; }
.acct.selected { border-color: var(--primary); box-shadow: 0 0 0 1px var(--primary); }
.acct { min-width: 0; border: 1px solid var(--border); border-radius: 14px; background: var(--card); overflow: hidden; box-shadow: 0 1px 2px hsl(222 40% 11% / .04); }
.acct-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 12px 13px; flex-wrap: wrap; }
.acct-id { display: flex; align-items: center; gap: 9px; min-width: 0; flex: 1; }
.acct-id > div { min-width: 0; }
.avatar { width: 34px; height: 34px; border-radius: 10px; background: var(--soft); border: 1px solid var(--border); display: flex; align-items: center; justify-content: center; font-weight: 700; font-size: 13px; color: var(--muted); flex: none; }
.acct-mail { font-weight: 650; font-size: 13px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.acct-meta { font-size: 11px; color: var(--muted); margin-top: 1px; }
.acct-actions { display: flex; align-items: center; gap: 7px; }
.acct-actions .switchline { gap: 5px; }
.acct-actions .hint { white-space: nowrap; }
.acct-actions button.ghost { padding: 5px 8px; }
.cells { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px; padding: 0 12px 12px; }
.cell { min-width: 0; border: 1px solid var(--border); border-radius: 10px; padding: 9px 10px; background: var(--bg); display: flex; flex-direction: column; gap: 7px; }
.cell-top { display: flex; align-items: center; justify-content: space-between; gap: 6px; flex-wrap: wrap; }
.cell-model { font-weight: 650; font-size: 13px; display: flex; align-items: center; gap: 7px; }
.dot { width: 8px; height: 8px; border-radius: 50%; background: var(--track); flex: none; }
.dot.ok { background: var(--ok); }
.dot.warn { background: var(--warn); }
.dot.bad { background: var(--bad); }
.countdown { white-space: nowrap; flex: none; }
.ticket-tags { display: flex; flex-wrap: wrap; gap: 3px; justify-content: flex-end; }
[hidden] { display: none !important; }
.cell-mid { display: flex; align-items: center; gap: 8px; font-size: 12px; color: var(--muted); font-variant-numeric: tabular-nums; }
.bar { height: 6px; background: var(--track); border-radius: 99px; overflow: hidden; flex: 1; min-width: 40px; }
.bar > i { display: block; height: 100%; background: var(--ok); border-radius: 99px; transition: width .5s; }
.bar.warn > i { background: var(--warn); }
.bar.bad > i { background: var(--bad); }
.bar.none { opacity: .4; }
.cell-actions { display: flex; gap: 4px; flex-wrap: wrap; }
.cell-actions button { padding: 4px 7px; font-size: 11px; border-radius: 7px; }
.tag { font-size: 10.5px; padding: 2px 8px; border-radius: 99px; background: var(--track); color: var(--muted); font-weight: 650; white-space: nowrap; }
.tag.ok { background: hsl(152 50% 34% / .13); color: var(--ok); }
.tag.warn { background: hsl(36 72% 40% / .16); color: var(--warn); }
.tag.bad { background: hsl(0 60% 48% / .13); color: var(--bad); }
.err { color: var(--bad); font-size: 11.5px; }
.cell .err { line-height: 1.35; word-break: break-all; }
.login { max-width: 380px; margin: 14vh auto; }
.login h1 { margin-bottom: 4px; }
.login label { margin: 14px 0; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.jobline { font-size: 12.5px; color: var(--muted); margin: 8px 0 0; overflow-wrap: anywhere; }
.empty { color: var(--muted); font-size: 13px; text-align: center; padding: 28px 0; }
.savebtn.unsaved { outline: 2px solid hsl(36 72% 40% / .55); outline-offset: 1px; }
.headline-actions { display: flex; gap: 8px; flex-wrap: wrap; }
@media (max-width: 1720px) {
  .account-grid[data-columns="4"] { grid-template-columns: repeat(3, minmax(0, 1fr)); }
}
@media (max-width: 1260px) {
  .account-grid[data-columns="4"], .account-grid[data-columns="3"] { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
@media (max-width: 900px) {
  .account-grid[data-columns="4"], .account-grid[data-columns="3"], .account-grid[data-columns="2"] { grid-template-columns: minmax(0, 1fr); }
  .selection-actions { margin-left: 0; }
}
@media (max-width: 620px) {
  header.page { padding: 16px 14px 2px; }
  main { padding: 12px 14px 32px; }
  .card { padding: 14px; }
  .grid.two { grid-template-columns: minmax(0, 1fr); }
  .account-grid { grid-template-columns: minmax(0, 1fr); }
  .acct-head { align-items: flex-start; }
  .acct-actions { width: 100%; justify-content: space-between; }
}
@media (max-width: 380px) { .cells { grid-template-columns: minmax(0, 1fr); } }
</style>
</head>
<body>
<div id="login" class="card login">
  <div class="brand-title"><img class="site-logo" src="/favicon.png" alt=""/><h1>Turn State Inject</h1></div>
  <p class="sub">使用管理后台同一把 Admin Key</p>
  <label>Admin Key <input id="key" type="password" autocomplete="current-password" onkeydown="if(event.key==='Enter'){event.preventDefault();saveKey();}"/></label>
  <button type="button" onclick="saveKey()" style="width:100%">进入</button>
  <p id="loginErr" class="err"></p>
</div>
<div id="app" style="display:none">
  <header class="page">
    <div>
      <div class="brand-title"><img class="site-logo" src="/favicon.png" alt=""/><h1>Turn State Inject</h1></div>
      <div class="sub">探测仅使用所选采集代理，不混用、不自动切换；业务代理池不动。票据按本地 60 分钟规则计时。</div>
    </div>
    <div class="headline-actions">
      <button class="ghost" onclick="load({fillCfg:true})">刷新页面</button>
      <button onclick="harvest(0,true)">探测全部账号</button>
      <button id="cancelJobBtn" class="ghost" onclick="cancelHarvest()" disabled>取消本轮</button>
    </div>
  </header>
  <main>
    <div class="grid two">
      <section class="card">
        <h2>注入与探测</h2>
        <div class="row" style="align-items:center">
          <div class="switchline">
            <button id="inject_enabled" class="switch" type="button" aria-label="注入开关" onclick="toggleBtn(this)"></button>
            <div>注入<div class="hint" id="inject_hint">关闭后业务请求不附带 turn-state</div></div>
          </div>
          <div class="switchline">
            <button id="auto_harvest" class="switch" type="button" aria-label="自动探测开关" onclick="toggleBtn(this)"></button>
            <div>自动探测<div class="hint">每分钟检查，按票据剩余时间续期</div></div>
          </div>
        </div>
        <p class="sub" style="margin:12px 0 0">开关及参数修改后，点击「保存全部设置」才会生效。</p>
      </section>
      <section class="card">
        <h2>探测参数</h2>
        <div class="row">
          <span class="sub">每分钟检查新账号、缺票据及临期格子</span>
          <label>每格尝试 <input id="max_attempts" type="number" min="1" max="50"/></label>
          <label>全局探测并发 <input id="concurrency" type="number" min="1" max="12" required/></label>
          <label>单账号模型并发 <input id="account_concurrency" type="number" min="1" max="4" required/></label>
          <button type="button" class="ghost" onclick="recommendedConcurrency()">推荐：4 / 2</button>
          <label>冷却（分钟） <input id="cooldown_minutes" type="number" min="1" max="1440"/></label>
          <label>提前续期（分钟） <input id="skip_ttl_minutes" type="number" min="0" max="55"/></label>
          <label>自动探测 · Pro 权重 <input id="plan_weight_pro" type="number" min="1" max="10" value="3" required/></label>
          <label>Pro Lite 权重 <input id="plan_weight_prolite" type="number" min="1" max="10" value="2" required/></label>
          <label>Plus 权重 <input id="plan_weight_plus" type="number" min="1" max="10" value="1" required/></label>
          <button type="button" class="ghost" onclick="resetPlanWeights()">恢复 3 / 2 / 1</button>
          <span class="sub">同等紧急程度内按账号加权；其他套餐权重 1。全设为 1 恢复同权。手动探测不加权，不改变业务请求调度或并发上限。</span>
          <label style="flex:1">模型列表 <input id="models" type="text" style="min-width:220px;width:100%"/></label>
        </div>
      </section>
    </div>
    <div class="settings-pair">
    <section class="card" aria-label="Astra 自动策略">
      <h2>Astra 自动策略</h2>
      <div class="settings-form">
        <label>共享普通未命中占比阈值（%） <input id="astra_miss_threshold_percent" type="number" min="1" max="100" step="1" value="80" required/></label>
        <label>分组降级复查间隔（分钟） <input id="astra_recheck_minutes" type="number" min="1" max="1440" value="30" required/></label>
        <div class="switchline full-width"><button id="astra_policy_enabled" class="switch" type="button" aria-label="自动分组策略开关" onclick="toggleBtn(this)"></button><div>自动迁组<div class="hint">默认关闭，只以 Astra 判定</div></div></div>
        <label>迁组失败批次阈值 <input id="astra_group_failure_batches" type="number" min="1" max="50" step="1" value="2" required/></label>
        <label>失败迁入分组 <select id="astra_failure_group_id"><option value="0">请选择 Codex 分组</option></select></label>
        <label>恢复迁入分组 <select id="astra_recovery_group_id"><option value="0">请选择 Codex 分组</option></select></label>
        <button type="button" class="ghost" onclick="loadGroups(true)">刷新分组列表</button>
        <div class="switchline full-width"><button id="astra_priority_policy_enabled" class="switch" type="button" aria-label="自动优先级策略开关" onclick="toggleBtn(this)"></button><div>自动调整调度优先级<div class="hint">独立开关，不要求选择分组</div></div></div>
        <label>优先级失败批次阈值 <input id="astra_priority_failure_batches" type="number" min="1" max="50" step="1" value="1" required/></label>
        <label>失败优先级 <input id="astra_failure_priority" type="number" min="-100" max="100" step="1" value="-1" required/></label>
        <label>恢复优先级 <input id="astra_recovery_priority" type="number" min="-100" max="100" step="1" value="0" required/></label>
      </div>
      <p id="astraPriorityWarning" class="sub" role="status" hidden>注意：失败优先级高于恢复优先级，会提高失败账号的调度顺序；仍可保存。</p>
      <p id="groupLoadMsg" class="sub" role="status"></p>
      <p class="sub">仅耗尽全部尝试的完整 Astra batch，正常有效且非 292 的响应占尝试上限比例达到共享阈值（默认 ≥80%），才符合失败条件。两个动作共享连续失败计数，分别达到自己的批次阈值时触发；迁组后计数仍可继续增长。网络异常、限流不算普通未命中；取消或 401 的 batch 不计失败。获得新 292 且确认成功才执行仍归策略所有的恢复动作。</p>
      <p class="sub">批次阈值在开始时固定；修改动作开关、阈值或目标后，旧批次不会执行策略，新计数重新开始，但不会清除已有动作归属。下方展示最近已记录批次的证据；实际共享连续失败次数以策略状态为准。</p>
      <p class="sub">迁组会替换全部分组，并进入仅 Astra 的低频复查；仅调整优先级不改变全模型采集。关闭动作不自动恢复；人工改组或调整优先级后，对应动作不再强行覆盖。自动复查需开启自动探测并参与探测。</p>
    </section>
    <section class="card" aria-label="采集代理">
      <h2>采集代理</h2>
      <div class="settings-form">
        <label class="full-width">代理提供商 <select id="harvest_proxy_provider" onchange="updateProxyPanels()"><option value="zooproxy">ZooProxy</option><option value="litport">Litport</option></select></label>
      </div>
      <p class="sub">每次仅使用所选提供商；不会混用或失败后切换。两套配置独立保留，切换和修改后统一保存；在途取票与确认保持原配置，后续尝试使用新配置。</p>
      <div id="zooproxy_panel">
      <div class="settings-form">
        <label class="full-width">地址 <input id="zoo_host" type="text"/></label>
        <label>用户前缀 <input id="zoo_user_prefix" type="text"/></label>
        <label>密码 <input id="zoo_password" type="password" placeholder="已保存则留空"/></label>
        <label>地区策略 <select id="zoo_region_mode"><option value="fixed">固定地区</option><option value="rotation">欧洲优先轮换</option></select></label>
        <label>固定地区 / 轮换首选欧洲地区 <input id="zoo_region" type="text" list="zoo_region_options" placeholder="默认 DE（德国）" autocomplete="off"/>
          <datalist id="zoo_region_options">
            <option value="DE">德国（默认）</option><option value="FR">法国</option><option value="GB">英国</option>
            <option value="NL">荷兰</option><option value="SE">瑞典</option><option value="FI">芬兰</option>
            <option value="CH">瑞士</option><option value="IE">爱尔兰</option>
            <option value="JP">日本</option><option value="KR">韩国</option><option value="SG">新加坡</option>
            <option value="HK">香港</option><option value="TW">台湾</option><option value="IN">印度</option>
            <option value="ID">印尼</option><option value="MY">马来西亚</option><option value="PH">菲律宾</option>
            <option value="TH">泰国</option><option value="VN">越南</option>
          </datalist>
        </label>
        <label>粘性（分钟） <input id="zoo_sticky_minutes" type="number" min="1" max="120"/></label>
      </div>
      <p class="sub">轮换：每 batch 前三段使用互不重复的欧洲地区，末段随机 JP / KR / TW。欧洲池：DE / FR / GB / NL / SE / FI / CH / IE；首选不在欧洲池时随机选择欧洲首站。</p>
      <p class="sub">每段基数为尝试上限 ÷ 4 向下取整（至少 1）；最后一段包含余数。20 次为 5/5/5/5，22 次为 5/5/5/7；不足 4 次不会到亚洲。取票与确认同地区、各自新 SID；在途取票与确认保持原配置，后续尝试使用新配置。</p>
      <p class="sub">地区代码为建议；实际可用性取决于 ZooProxy 账号与套餐。固定模式也可输入其他地区代码。ZooProxy 保持旧行为：取票与确认各自使用不同的新 SID。</p>
      </div>
      <div id="litport_panel" hidden>
      <div class="settings-form">
        <label class="full-width">地址 <input id="litport_host" type="text" value="hub-us-10.litport.net:1337" autocomplete="off"/></label>
        <label>用户名 <input id="litport_username" type="text" autocomplete="off" placeholder="填写你的 Litport 用户名"/></label>
        <label>密码 <input id="litport_password" type="password" autocomplete="new-password" placeholder="已保存则留空"/></label>
        <label>地区策略 <select id="litport_region_mode"><option value="fixed">固定地区</option><option value="rotation">欧洲优先轮换</option></select></label>
        <label>固定地区 / 轮换首选欧洲地区 <input id="litport_region" type="text" value="DE" list="litport_region_options" autocomplete="off"/>
          <datalist id="litport_region_options"><option value="DE">德国（默认）</option><option value="FR">法国</option><option value="GB">英国</option><option value="NL">荷兰</option><option value="JP">日本</option></datalist>
        </label>
        <label>会话 TTL（秒） <input id="litport_session_seconds" type="number" min="1" max="86400" step="1" value="600" required/></label>
      </div>
      <p class="sub">Litport 每次尝试生成新的 12 位 SID，同一次尝试的取票与确认复用同一 SID；TTL 单位为秒（1–86400，默认 600），不是票据有效期。</p>
      <p class="sub">固定模式默认 DE。轮换模式使用 DE / FR / GB / NL 欧洲池，末段 JP；首选欧洲地区优先，不在欧洲池时随机选择欧洲首站。ZooProxy 的轮换策略不变；切换提供商或地区设置后，后续尝试重建地区计划。</p>
      </div>
      <p class="sub">密码不会回显；两家密码留空均保留已保存值。未选中的提供商配置也会一起保存。</p>
    </section>
    </div>
    <section class="settings-actions" aria-label="保存全部设置">
      <div>
        <strong>统一保存设置</strong>
        <p class="sub">保存注入开关、探测参数、Astra 自动策略和两套采集代理配置；卡片布局与筛选即时生效。</p>
        <p id="cfgMsg" class="sub" role="status" aria-live="polite">修改后点击右侧按钮保存。</p>
      </div>
      <button id="saveBtn" type="button" class="savebtn" onclick="saveCfg()">保存全部设置</button>
    </section>
    <section class="card" aria-label="账号池总览">
      <div class="pool-head">
        <div><h2>账号池</h2><div class="sub">按账号展示各模型票据状态</div></div>
        <div class="row" id="stats">
          <div class="stat"><b id="st_total">-</b><span>账号数</span></div>
          <div class="stat"><b id="st_full">-</b><span>满血 292</span></div>
          <div class="stat"><b id="st_live">-</b><span>存活 ticket</span></div>
          <div class="stat"><b id="st_exh">-</b><span>耗尽 / 冷却</span></div>
        </div>
      </div>
      <div class="pool-tools">
        <label>搜索账号 <input id="accountSearch" type="text" placeholder="邮箱或账号 ID" oninput="applyFilters()"/></label>
        <label>状态筛选 <select id="accountFilter" onchange="applyFilters()"><option value="all">全部</option><option value="missing">缺有效票据</option><option value="expiring">即将过期（≤5分钟）</option><option value="active">排队 / 探测中</option><option value="demoted">自动降级</option><option value="error">策略错误</option></select></label>
        <label>套餐筛选 <select id="accountPlanFilter" onchange="applyFilters()"><option value="all">全部套餐</option><option value="pro">Pro</option><option value="prolite">Pro Lite</option><option value="plus">Plus</option><option value="other">其他 / 未知</option></select></label>
        <span id="filterCount" class="hint"></span>
        <span class="hint">下方批量操作包含全部已选账号（含筛选隐藏项）。</span>
      </div>
      <div class="pool-tools">
        <span class="hint">每行卡片</span>
        <div class="segment" role="group" aria-label="每行账号卡片列数">
          <button type="button" data-column-choice="auto" aria-pressed="true" onclick="setColumns('auto')">自动</button>
          <button type="button" data-column-choice="1" aria-pressed="false" onclick="setColumns('1')">1 列</button>
          <button type="button" data-column-choice="2" aria-pressed="false" onclick="setColumns('2')">2 列</button>
          <button type="button" data-column-choice="3" aria-pressed="false" onclick="setColumns('3')">3 列</button>
          <button type="button" data-column-choice="4" aria-pressed="false" onclick="setColumns('4')">4 列</button>
        </div>
        <label class="switchline" style="flex-direction:row;gap:6px"><input id="selectAll" class="select-account" type="checkbox" onchange="toggleAllAccounts(this.checked)"/>全选筛选结果</label>
        <div class="selection-actions">
          <span id="selectionCount" class="hint">已选择 0 个</span>
          <button class="ghost" type="button" data-selection-action onclick="clearSelection()" disabled>取消选择</button>
          <button type="button" data-selection-action onclick="harvestSelected()" disabled>刷新选中账号</button>
          <button class="ghost" type="button" data-selection-action onclick="setSelectedHarvest(true)" disabled>开启选中探测</button>
          <button class="ghost" type="button" data-selection-action onclick="setSelectedHarvest(false)" disabled>暂停选中探测</button>
        </div>
      </div>
      <div class="pool-progress" role="progressbar" id="poolProgress" aria-label="账号池进度" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0">
        <div class="pool-progress-label"><span id="poolProgressTitle">可用票据覆盖率</span><strong id="poolProgressText">0/0 · 0%</strong></div>
        <div class="pool-progress-track"><i class="pool-progress-fill" id="poolProgressFill"></i></div>
      </div>
      <p id="breakerStatus" class="sub" role="status"></p>
      <section id="jobPanel" hidden>
        <div class="pool-progress" role="progressbar" id="taskProgress" aria-label="本轮任务处理进度" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0">
          <div class="pool-progress-label"><span>本轮任务 · <strong id="taskTitle"></strong></span><strong id="taskProgressText">0/0 · 0%</strong></div>
          <div class="pool-progress-track"><i class="pool-progress-fill task" id="taskProgressFill"></i></div>
        </div>
        <p id="jobMsg" class="jobline"></p>
        <div class="pool-tools"><span id="jobTime" class="sub"></span><button id="dismissJobBtn" class="ghost" type="button" onclick="dismissJob()" hidden>关闭摘要</button></div>
      </section>
      <p id="actionMsg" class="sub" role="status" aria-live="polite"></p>
      <p id="connectionMsg" class="err" role="status"></p>
    </section>
    <div id="accounts" class="account-grid"></div>
    <div id="emptyTip" class="empty" style="display:none">没有可探测的账号</div>
  </main>
</div>
<script>
const KEY = 'admin_key';
const COLUMNS_KEY = 'inject_account_columns';
const selectedAccounts = new Set();
const accountViews = new Map();
let visibleAccountIDs = new Set();
function harvestPlanClass(value) {
  const plan=typeof value==='string'?value.trim().toLowerCase():'';
  if(['prolite','pro_lite','pro-lite'].includes(plan))return 'prolite';
  return plan==='pro'||plan==='plus'?plan:'other';
}
function applyFilters() {
  if(!data)return;
  const now=serverNow(), query=$('accountSearch').value.trim().toLowerCase(), mode=$('accountFilter').value;
  const planFilter=$('accountPlanFilter').value;
  const activeIDs=new Set(activeJob(data.job)?(data.job.cells||[]).filter(c=>['queued','running','confirming','retrying'].includes(c.phase)).map(c=>c.account_id):[]);
  const visible=new Set();
  (data.accounts||[]).forEach(a=> {
    const tickets=a.tickets||[];
    const missing=(data.config?.models||[]).some(model=>!tickets.some(t=>t.model.toLowerCase()===model.toLowerCase()&&t.length===292&&t.token&&remaining(t,now)>0));
    const expiring=tickets.some(t=>t.length===292&&t.token&&remaining(t,now)>0&&remaining(t,now)<=300);
    const state=mode==='all'||mode==='missing'&&missing||mode==='expiring'&&expiring||mode==='active'&&activeIDs.has(a.id)||mode==='demoted'&&a.astra_policy?.demoted||mode==='error'&&!!a.astra_policy?.error;
    const match=(!query||(a.email||'').toLowerCase().includes(query)||String(a.id).includes(query))&&state&&(planFilter==='all'||harvestPlanClass(a.plan_type)===planFilter);
    if(match)visible.add(a.id);
    const view=accountViews.get(a.id);if(view)view.node.hidden=!match;
  });
  visibleAccountIDs=visible;
  text($('filterCount'),'显示 '+visible.size+' / '+(data.accounts||[]).length+' 个账号');
  $('emptyTip').style.display=visible.size?'none':'block';
  text($('emptyTip'),data.accounts?.length?'没有匹配的账号':'没有可探测的账号');
  syncSelection();
}
function renderBreaker() {
  const b=data?.breaker;
  if(!b){text($('breakerStatus'),'');return;}
  const state=b.state;
  const message=state==='open'?'网络熔断：暂停新探测，'+fmtRemain((b.retry_at||0)-serverNow())+' 后尝试恢复':state==='half_open'?'网络恢复检查中：仅放行一个探测单元':'探测网络正常';
  text($('breakerStatus'),message+' · 窗口 '+(b.failures||0)+'/'+(b.samples||0)+' 次网络失败 · '+(b.accounts||0)+' 个账号');
  $('breakerStatus').className=state==='closed'?'sub':'tag warn';
}
let data = null;
let cfgDirty = false;
let cfgEditRevision = 0;
let columnsChoice = 'auto';
let clockAnchor = {unix: Date.now()/1000, mono: performance.now()};
let loading = false;
let pendingLoad = false;
let revision = 0;
let mutationCount = 0;
let mutationQueue = Promise.resolve();
let dismissedJob = '';
let groupsLoading = false;
let groupsLoaded = false;
let groupsAttempted = false;
let availableGroups = [];
function setGroupOptions(id, selected) {
  const select=document.getElementById(id);
  const chosen=String(selected || '0');
  select.replaceChildren(new Option('请选择 Codex 分组','0'));
  availableGroups.forEach(g=>select.add(new Option(g.name+' · #'+g.id,String(g.id))));
  if(chosen!=='0' && !availableGroups.some(g=>String(g.id)===chosen))select.add(new Option('当前分组 #'+chosen+'（待加载或已不可用）',chosen));
  select.value=chosen;
}
async function loadGroups(force=false) {
  if(groupsLoading || (groupsLoaded && !force))return;
  groupsLoading=true; groupsAttempted=true;
  try {
    const result=await api('/account-groups');
    availableGroups=(result.groups||[]).filter(g=>!g.channel || g.channel.toLowerCase()==='codex');
    for(const id of ['astra_failure_group_id','astra_recovery_group_id'])setGroupOptions(id,$(id).value);
    groupsLoaded=true; text($('groupLoadMsg'),'已读取 '+availableGroups.length+' 个 Codex 分组。修改后点击保存全部设置。');
  } catch(e) {text($('groupLoadMsg'),'分组加载失败：'+e.message+'；可点击刷新重试。');}
  finally {groupsLoading=false;}
}
try {
  const saved = localStorage.getItem(COLUMNS_KEY);
  if (['auto','1','2','3','4'].includes(saved)) columnsChoice = saved;
  dismissedJob = sessionStorage.getItem('inject_dismissed_job') || '';
} catch (e) {}
const $ = id => document.getElementById(id);
function text(el, value) { const next = String(value ?? ''); if (el.textContent !== next) el.textContent = next; }
function serverNow() { return clockAnchor.unix + (performance.now()-clockAnchor.mono)/1000; }
function remaining(t, now=serverNow()) { return t && t.issued_unix > 0 ? Math.ceil(t.issued_unix+3600-now) : null; }
function fmtRemain(sec) {
  if (sec == null) return '无票据';
  const n = Math.max(0, Math.floor(sec));
  return Math.floor(n/60)+':'+String(n%60).padStart(2,'0');
}
function shortModel(model) { return model.split('-').pop() || model; }
function activeJob(job) { return !!job && ['running','cancelling'].includes(job.status); }
function setColumns(value) {
  if (!['auto','1','2','3','4'].includes(value)) return;
  columnsChoice = value;
  if (value === 'auto') $('accounts').removeAttribute('data-columns');
  else $('accounts').dataset.columns = value;
  document.querySelectorAll('[data-column-choice]').forEach(button => button.setAttribute('aria-pressed', String(button.dataset.columnChoice === value)));
  try { localStorage.setItem(COLUMNS_KEY, value); } catch (e) {}
}
function syncSelection() {
  const accounts = data?.accounts || [];
  const ids = new Set(accounts.map(a=>a.id));
  for (const id of selectedAccounts) if (!ids.has(id)) selectedAccounts.delete(id);
  const all = $('selectAll');
  const visibleSelected=[...visibleAccountIDs].filter(id=>selectedAccounts.has(id)).length;
  all.checked = visibleAccountIDs.size>0 && visibleSelected===visibleAccountIDs.size;
  all.indeterminate = visibleSelected>0 && !all.checked;
  all.disabled = !visibleAccountIDs.size;
  text($('selectionCount'), '已选择 '+selectedAccounts.size+' 个（隐藏 '+(selectedAccounts.size-visibleSelected)+' 个）');
  document.querySelectorAll('[data-selection-action]').forEach(b=> { b.disabled = !selectedAccounts.size; });
  accountViews.forEach((view,id)=> {
    view.select.checked = selectedAccounts.has(id);
    view.node.classList.toggle('selected', selectedAccounts.has(id));
  });
}
function toggleAccount(id, checked) {
  if (checked) selectedAccounts.add(id); else selectedAccounts.delete(id);
  syncSelection();
}
function toggleAllAccounts(checked) {
  for(const id of visibleAccountIDs) {if(checked)selectedAccounts.add(id);else selectedAccounts.delete(id);}
  syncSelection();
}
function clearSelection() { selectedAccounts.clear(); syncSelection(); }
function notice(message, error=false) {
  text($('actionMsg'), message);
  $('actionMsg').className = error ? 'err' : 'sub';
}
function headers() {
  const h = {'Content-Type':'application/json'};
  const key = localStorage.getItem(KEY);
  if (key) h['X-Admin-Key'] = key;
  return h;
}
async function api(path, opts={}) {
  const response = await fetch('/api/admin'+path, Object.assign({headers:headers(), cache:'no-store'},opts));
  if (response.status === 401) {
    localStorage.removeItem(KEY);
    $('login').style.display = 'block'; $('app').style.display = 'none';
    throw new Error('unauthorized');
  }
  const raw = await response.text();
  let body;
  try { body = raw ? JSON.parse(raw) : {}; } catch (e) { body = {error:raw}; }
  if (!response.ok) throw new Error(body.error || raw || String(response.status));
  return body;
}
// Serialize local writes. In-flight overview responses from before any write
// are discarded, so they cannot roll back a newer job/config/ticket action.
function mutate(work) {
  revision++;
  mutationCount++;
  const run = mutationQueue.then(work);
  mutationQueue = run.catch(()=>{});
  return run.catch(e=>notice(e.message === 'unauthorized' ? '密钥无效' : e.message, true)).finally(()=> {
    revision++;
    mutationCount--;
    if (!mutationCount) load();
  });
}
async function load(opts={}) {
  if (opts.fillCfg && cfgDirty && !opts.initial) {
    notice('配置有未保存修改，已保留输入；仅刷新账号状态。');
    opts.fillCfg = false;
  }
  if (loading || mutationCount) { pendingLoad = true; return; }
  loading = true;
  const startedRevision = revision;
  const began = performance.now();
  try {
    const next = await api('/turn-state');
    if (revision !== startedRevision || mutationCount) { pendingLoad = true; return; }
    data = next;
    const received = performance.now();
    clockAnchor = {unix:Number(next.now_unix) || Date.now()/1000, mono:(began+received)/2};
    if ((opts.fillCfg && !cfgDirty) || !document.documentElement.dataset.cfgLoaded) {
      fillCfg(next.config || {});
      document.documentElement.dataset.cfgLoaded = '1';
    }
    render();
    $('login').style.display='none'; $('app').style.display='block';
    if(!groupsAttempted)loadGroups();
    text($('loginErr'),''); text($('connectionMsg'),'');
  } catch(e) {
    if (!data || e.message === 'unauthorized') {
      $('login').style.display='block'; $('app').style.display='none';
      text($('loginErr'),e.message === 'unauthorized' ? '密钥无效' : e.message);
    } else text($('connectionMsg'),'状态更新失败：'+e.message+'；保留上次数据，稍后重试。');
  } finally {
    loading=false;
    if (pendingLoad && !mutationCount) { pendingLoad=false; setTimeout(()=>load(),0); }
  }
}
function saveKey() {
  const key=$('key').value.trim();
  if (!key) { text($('loginErr'),'请输入 Admin Key'); return; }
  localStorage.setItem(KEY,key);
  load({fillCfg:true,initial:true});
}
function setSwitch(id,on) {
  const el=$(id); el.classList.toggle('on',!!on); el.dataset.on=on?'1':'0';
  el.setAttribute('aria-pressed',String(!!on));
}
function toggleBtn(el) {
  setSwitch(el.id,el.dataset.on!=='1');
  updateInjectHint(); markCfgDirty();
}
function updateInjectHint() {
  text($('inject_hint'),$('inject_enabled').dataset.on === '1' ? '有可用票据时自动附带 turn-state' : '关闭代理额外注入，不影响客户端自带状态');
}
const configInputs=['plan_weight_pro','plan_weight_prolite','plan_weight_plus','max_attempts','concurrency','account_concurrency','cooldown_minutes','skip_ttl_minutes','models','harvest_proxy_provider','zoo_host','zoo_user_prefix','zoo_password','zoo_region','zoo_region_mode','zoo_sticky_minutes','litport_host','litport_username','litport_password','litport_region','litport_region_mode','litport_session_seconds','astra_failure_group_id','astra_recovery_group_id','astra_recheck_minutes','astra_miss_threshold_percent','astra_group_failure_batches','astra_priority_failure_batches','astra_failure_priority','astra_recovery_priority'];
function updateProxyPanels() {
  const provider=$('harvest_proxy_provider').value;
  $('zooproxy_panel').hidden=provider!=='zooproxy';
  $('litport_panel').hidden=provider!=='litport';
}
function fillCfg(c) {
  setSwitch('inject_enabled',c.inject_enabled); setSwitch('auto_harvest',c.auto_harvest);
  setSwitch('astra_policy_enabled',c.astra_policy_enabled);
  setSwitch('astra_priority_policy_enabled',c.astra_priority_policy_enabled);
  for(const [id,fallback] of Object.entries({astra_group_failure_batches:2,astra_priority_failure_batches:1,astra_failure_priority:-1,astra_recovery_priority:0}))$(id).value=c[id]??fallback;
  for(const id of ['astra_failure_group_id','astra_recovery_group_id'])setGroupOptions(id,c[id]);
  $('astra_recheck_minutes').value=c.astra_recheck_minutes||30;
  $('astra_miss_threshold_percent').value=c.astra_miss_threshold_percent??80;
  updateInjectHint();
  configInputs.filter(id=>!['models','zoo_password','litport_password'].includes(id)).forEach(id=> {
    if (c[id] != null) $(id).value=c[id];
  });
  $('harvest_proxy_provider').value=c.harvest_proxy_provider || 'zooproxy';
  $('litport_host').value=c.litport_host ?? 'hub-us-10.litport.net:1337';
  $('litport_username').value=c.litport_username ?? '';
  $('litport_region').value=c.litport_region ?? 'DE';
  $('litport_region_mode').value=c.litport_region_mode==='rotation'?'rotation':'fixed';
  $('litport_session_seconds').value=c.litport_session_seconds ?? 600;
  updateProxyPanels();
  $('zoo_region_mode').value=c.zoo_region_mode==='rotation'?'rotation':'fixed';
  $('plan_weight_pro').value=c.plan_weight_pro||3;
  $('plan_weight_prolite').value=c.plan_weight_prolite||2;
  $('plan_weight_plus').value=c.plan_weight_plus||1;
  if (!c.account_concurrency) $('account_concurrency').value=1;
  $('models').value=(c.models||[]).join(', ');
  $('zoo_password').placeholder=c.zoo_password_set?'已保存，留空不改':'必填才能探测';
  $('litport_password').placeholder=c.litport_password_set?'已保存，留空不改':'必填才能探测';
  updateAstraPriorityWarning();
}
function updateAstraPriorityWarning() {
  $('astraPriorityWarning').hidden=!(Number($('astra_failure_priority').value)>Number($('astra_recovery_priority').value));
}
function markCfgDirty() { updateAstraPriorityWarning(); cfgEditRevision++; cfgDirty=true; $('saveBtn').classList.add('unsaved'); text($('cfgMsg'),'有未保存修改'); }
function recommendedConcurrency() { $('concurrency').value=4; $('account_concurrency').value=2; markCfgDirty(); }
function resetPlanWeights() {
  $('plan_weight_pro').value=3; $('plan_weight_prolite').value=2; $('plan_weight_plus').value=1; markCfgDirty();
}
function saveCfg() {
  const body={
    inject_enabled:$('inject_enabled').dataset.on==='1', auto_harvest:$('auto_harvest').dataset.on==='1',
    interval_minutes:data?.config?.interval_minutes || 50,
    models:$('models').value.split(',').map(s=>s.trim()).filter(Boolean),
    astra_policy_enabled:$('astra_policy_enabled').dataset.on==='1',
    astra_priority_policy_enabled:$('astra_priority_policy_enabled').dataset.on==='1',
    astra_failure_group_id:Number($('astra_failure_group_id').value),
    astra_recovery_group_id:Number($('astra_recovery_group_id').value),
  };
  if(body.astra_policy_enabled) {
    const fail=body.astra_failure_group_id, recovery=body.astra_recovery_group_id;
    if(!fail || !recovery || fail===recovery) {notice('请选择两个不同的失败/恢复目标分组。',true);return;}
  }
  if((body.astra_policy_enabled||body.astra_priority_policy_enabled) && !body.models.some(m=>m.toLowerCase()==='gpt-6-astra')) {notice('启用 Astra 策略需要在模型列表中包含 gpt-6-astra。',true);return;}
  for (const id of ['plan_weight_pro','plan_weight_prolite','plan_weight_plus','max_attempts','concurrency','account_concurrency','cooldown_minutes','skip_ttl_minutes','zoo_sticky_minutes','litport_session_seconds','astra_recheck_minutes','astra_miss_threshold_percent','astra_group_failure_batches','astra_priority_failure_batches','astra_failure_priority','astra_recovery_priority']) {
    if (!$(id).reportValidity()) {
      if(id==='litport_session_seconds')notice('Litport 会话 TTL 必须为 1–86400 的整数秒；请切换到 Litport 检查配置。',true);
      return;
    }
    body[id]=Number($(id).value);
  }
  for(const [id,min,max] of [['astra_group_failure_batches',1,50],['astra_priority_failure_batches',1,50],['astra_failure_priority',-100,100],['astra_recovery_priority',-100,100]]) {
    if(!$(id).value.toString().trim() || !Number.isInteger(body[id]) || body[id]<min || body[id]>max) {notice(id+' 必须为 '+min+'–'+max+' 的整数。',true);return;}
  }
  updateAstraPriorityWarning();
  for (const id of ['harvest_proxy_provider','zoo_host','zoo_user_prefix','zoo_region','zoo_region_mode','litport_host','litport_username','litport_region','litport_region_mode']) body[id]=$(id).value.trim();
  if(!['zooproxy','litport'].includes(body.harvest_proxy_provider)) {notice('请选择有效的采集代理提供商。',true);return;}
  body.zoo_password=$('zoo_password').value;
  body.litport_password=$('litport_password').value;
  const savedRevision=cfgEditRevision;
  text($('cfgMsg'),'等待保存…');
  return mutate(async()=> {
    text($('cfgMsg'),'正在保存全部设置…');
    try {
      const cfg=await api('/settings/turn-state',{method:'PUT',body:JSON.stringify(body)});
      data.config=cfg;
      if(savedRevision===cfgEditRevision) {
        $('zoo_password').value=''; $('litport_password').value=''; cfgDirty=false;
        $('saveBtn').classList.remove('unsaved'); fillCfg(cfg); text($('cfgMsg'),'全部设置已保存');
      } else text($('cfgMsg'),'已保存提交的设置；后续修改仍待保存。');
    } catch(e) {
      text($('cfgMsg'),'保存失败：'+(e.message==='unauthorized'?'密钥无效':e.message));
      throw e;
    }
  });
}
function queueHarvest(body) {
  return mutate(async()=> {
    data.job=await api('/turn-state/harvest',{method:'POST',body:JSON.stringify(body)});
    dismissedJob=''; render(); notice('已提交调度；重复格子自动合并，等待可用并发名额。');
  });
}
function harvest(id,force,model) { return queueHarvest({account_id:id||0,model:model||'',force:!!force}); }
function harvestSelected() { if (selectedAccounts.size) return queueHarvest({account_ids:[...selectedAccounts],force:true}); }
function cancelHarvest() {
  return mutate(async()=> {
    await api('/turn-state/harvest/cancel',{method:'POST',body:'{}'});
    notice('已请求取消当前轮次；自动探测开启时，后续扫描仍会重新检查缺票据格子。');
  });
}
function setSelectedHarvest(enabled) {
  const ids=[...selectedAccounts]; if (!ids.length) return;
  return mutate(async()=> {
    data.config=await api('/turn-state/accounts/harvest',{method:'PUT',body:JSON.stringify({account_ids:ids,enabled})});
    data.accounts.forEach(a=>{if(ids.includes(a.id))a.harvest_enabled=enabled;});
    render(); notice((enabled?'已开启':'已暂停')+ids.length+' 个账号参与探测');
  });
}
function toggleHarvest(id,enabled) {
  return mutate(async()=> {
    data.config=await api('/turn-state/accounts/'+id+'/harvest',{method:'PUT',body:JSON.stringify({enabled})});
    const a=data.accounts.find(a=>a.id===id); if(a)a.harvest_enabled=enabled;
    render();
  });
}
function clearT(id,model) {
  return mutate(async()=> {
    await api('/turn-state/accounts/'+id+'/models/'+encodeURIComponent(model),{method:'DELETE'});
    const a=data.accounts.find(a=>a.id===id);
    if(a)a.tickets=(a.tickets||[]).filter(t=>t.model!==model);
    render(); notice('票据已清除；开启自动探测时，后续扫描会重新补票。');
  });
}
async function copyToken(id,model,btn) {
  const ticket=data?.accounts?.find(a=>a.id===id)?.tickets?.find(t=>t.model===model);
  if(!ticket?.token)return;
  try {
    await navigator.clipboard.writeText(ticket.token);
    text(btn,'已复制'); setTimeout(()=>text(btn,'复制票据'),1400);
  } catch(e) { notice('复制失败：请检查浏览器剪贴板权限。',true); }
}
function makeCard(a) {
  const node=document.createElement('section'); node.className='acct'; node.dataset.accountId=a.id;
  node.innerHTML='<div class="acct-head"><div class="acct-id"><input class="select-account" type="checkbox"/><span class="avatar"></span><div><div class="acct-mail"></div><div class="acct-meta"></div></div></div><div class="acct-actions"><div class="switchline"><button class="switch" type="button" aria-label="探测开关"></button><span class="hint"></span></div><button class="ghost" type="button">刷新此号</button></div></div><div class="policy-status" hidden></div><div class="cells"></div>';
  const view={node,select:node.querySelector('input'),avatar:node.querySelector('.avatar'),mail:node.querySelector('.acct-mail'),meta:node.querySelector('.acct-meta'),toggle:node.querySelector('.switch'),hint:node.querySelector('.hint'),policy:node.querySelector('.policy-status'),cells:node.querySelector('.cells'),models:new Map(),account:a};
  view.select.onchange=()=>toggleAccount(a.id,view.select.checked);
  view.toggle.onclick=()=>toggleHarvest(a.id,!view.account.harvest_enabled);
  node.querySelector('.acct-actions > button').onclick=()=>harvest(a.id,true);
  return view;
}
function renderPolicy(view) {
  const p=view.account.astra_policy;
  view.policy.hidden=!p || (!p.enabled && !p.demoted && !p.priority_demoted && !p.group_triggered && !p.priority_triggered && !p.group_suppressed && !p.priority_suppressed && !p.consecutive_failures && !p.error && !p.last_outcome && !p.latest_batch);
  if(!p)return;
  const outcomes={ordinary_miss:'本批符合普通未命中失败条件',new_292:'获得新292',inconclusive:'本批未计失败（异常或中断）',miss_batch:'正常未命中 batch',confirmed_292:'292 确认成功',success:'获得292',demoted:'已自动迁组',recovered:'已自动恢复',manual_override:'人工调整分组'};
  const cfg=data?.config||{};
  const parts=[p.enabled?'Astra 策略开启':'Astra 策略关闭', '共享连续失败 '+(p.consecutive_failures||0)+'（迁组阈值 '+(cfg.astra_group_failure_batches??2)+'，优先级阈值 '+(cfg.astra_priority_failure_batches??1)+'）'];
  const state=(fired,owned,suppressed)=>[fired?'已触发':'未触发',owned?'策略持有':'未持有',suppressed?'人工覆盖后抑制':'未抑制'].join(' / ');
  const groupName=id=>availableGroups.find(g=>g.id===id)?.name || (id?'#'+id:'未配置');
  parts.push('迁组'+(cfg.astra_policy_enabled?'开启':'关闭')+'：'+state(p.group_triggered||p.demoted,p.demoted,p.group_suppressed)+'；目标 '+groupName(cfg.astra_failure_group_id)+' → '+groupName(cfg.astra_recovery_group_id));
  parts.push('优先级'+(cfg.astra_priority_policy_enabled?'开启':'关闭')+'：'+state(p.priority_triggered||p.priority_demoted,p.priority_demoted,p.priority_suppressed)+'；目标 '+(cfg.astra_failure_priority??-1)+' → '+(cfg.astra_recovery_priority??0));
  if(p.priority_demoted)parts.push('策略持有的优先级 '+(p.failure_priority??'—'));
  if(p.error) {
    const errors={manual_membership_changed:'人工分组已变更，迁组动作归属已解除，不影响共享失败计数或优先级动作',manual_priority_changed:'人工优先级已变更，优先级动作归属已解除，不影响共享失败计数或迁组动作',policy_state_unavailable:'策略状态暂不可用'};
    parts.push(errors[p.error] || '策略错误：'+p.error);
  }
  if(p.demoted) {
    const group=availableGroups.find(g=>g.id===p.failure_group_id);
    parts.push('已迁入 '+(group?.name||'#'+p.failure_group_id));
    if(p.next_recovery_at)parts.push('下次复查 '+new Date(p.next_recovery_at*1000).toLocaleString());
  }
  const batch=p.latest_batch;
  if(batch) {
    const reasons={qualified_miss:'符合普通未命中失败条件',insufficient_misses:'普通未命中占比不足，不计失败',success:'获得292，不计失败',confirmation_warning:'292 确认警告，不计失败',interrupted:'异常或中断，不计失败',not_exhausted:'未耗尽全部尝试，不计失败'};
    parts.push('最近一批：尝试 '+(batch.attempts??'—')+'/'+(batch.max_attempts??'—')+'，普通未命中 '+(batch.ordinary_misses??'—')+'，批次阈值 '+(batch.threshold_percent??'—')+'%，'+(batch.exhausted?'已耗尽':'未耗尽'));
    parts.push('批次判定：'+(reasons[batch.reason]||'未知'));
    if(batch.reason==='qualified_miss')parts.push('符合条件不代表已计数，实际连续失败以上方状态为准');
  } else if(p.last_outcome) {
    parts.push('最近一批计数不可用（旧版状态），是否符合当前阈值未知');
  }
  // A membership guard rejection takes precedence over the stored outcome.
  // Batch evidence describes qualification, never whether the streak advanced.
  if(p.last_outcome && !p.error && (batch || !['ordinary_miss','miss_batch'].includes(p.last_outcome)))parts.push(outcomes[p.last_outcome]||p.last_outcome);
  text(view.policy,parts.join(' · '));view.policy.title=parts.join('\n');
}
function makeCell(id,model) {
  const node=document.createElement('div');node.className='cell';
  node.innerHTML='<div class="cell-top"><span class="cell-model"><span class="dot"></span><span class="model-label"></span></span><span class="ticket-tags"><span class="tag kind"></span><span class="tag warn confirmation" hidden>确认警告</span><span class="tag lifecycle" hidden></span></span></div><div class="cell-mid"><div class="bar"><i></i></div><span class="countdown"></span></div><div class="sub phase" hidden></div><div class="err" hidden></div><div class="cell-actions"><button class="ghost" type="button">复制票据</button><button class="ghost" type="button">刷新</button><button class="danger" type="button">清除</button></div>';
  const view={node,dot:node.querySelector('.dot'),kind:node.querySelector('.kind'),confirm:node.querySelector('.confirmation'),lifecycle:node.querySelector('.lifecycle'),bar:node.querySelector('.bar'),fill:node.querySelector('.bar i'),countdown:node.querySelector('.countdown'),phase:node.querySelector('.phase'),error:node.querySelector('.err'),ticket:null};
  const buttons=node.querySelectorAll('button'); view.copy=buttons[0];
  view.copy.onclick=()=>copyToken(id,model,view.copy); buttons[1].onclick=()=>harvest(id,true,model);buttons[2].onclick=()=>clearT(id,model);
  text(node.querySelector('.model-label'),shortModel(model)); node.querySelector('.cell-model').title=model;
  return view;
}
function updateTicket(view,t,live) {
  view.ticket=t;
  const length=Number(t.length)||0;
  view.kind.className='tag kind '+(length===292?'ok':length?'bad':'');
  text(view.kind,length===292?'满血 292':length?'降级 '+length:'无票据');
  view.confirm.hidden=!t.confirm_warning;
  view.copy.disabled=!t.token;
  const running=activeJob(data.job) && live && ['queued','running','confirming','retrying'].includes(live.phase);
  view.phase.hidden=!running;
  const provider=live?.provider==='zooproxy'?'ZooProxy':live?.provider==='litport'?'Litport':'';
  const region=typeof live?.region==='string' && /^[A-Za-z]{2}$/.test(live.region)?live.region.toUpperCase():'';
  const phases={queued:'排队',running:'探测中',confirming:'确认',retrying:'等待重试'};
  const phaseText=running?phases[live.phase]+' · 尝试 '+live.attempt+'/'+live.max+(provider?' · '+provider:'')+(region?' · 请求地区 '+region:''):'';
  text(view.phase,phaseText);
  // Do not expose raw diagnostic details, proxy URLs, credentials or session IDs.
  view.phase.title=phaseText;
  view.error.hidden=!t.last_error;
  text(view.error,t.last_error||''); view.error.title=t.last_error||'';
}
function updateClock(view,now) {
  const t=view.ticket || {};
  const rem=remaining(t,now);
  text(view.countdown,rem==null?'无票据':rem>0?fmtRemain(rem)+' 剩余':'已过期');
  view.bar.className='bar'+(rem==null?' none':rem<=0?' bad':rem<=600?' warn':'');
  view.fill.style.width=(rem==null?0:Math.max(0,Math.min(100,rem/36)))+'%';
  view.dot.className='dot'+(!t.length?'':rem!=null&&rem<=0?' bad':t.exhausted?' warn':t.length===292?' ok':' bad');
  const cooling=t.cooldown_until>now;
  view.lifecycle.hidden=!t.exhausted && !cooling;
  view.lifecycle.className='tag lifecycle warn';
  text(view.lifecycle,cooling?'冷却 '+fmtRemain(t.cooldown_until-now):t.exhausted?'尝试耗尽':'');
}
function setProgress(id,done,total) {
  const pct=total>0?Math.min(100,Math.max(0,Math.round(done/total*100))):0;
  text($(id+'Text'),done+'/'+total+' · '+pct+'%');
  $(id+'Fill').style.width=pct+'%';$(id).setAttribute('aria-valuenow',String(pct));
}
function tick() {
  if(!data)return;
  const now=serverNow(); let full=0,live=0,exh=0,total=0;
  accountViews.forEach(view=>view.models.forEach(cell=> {
    updateClock(cell,now);total++;
    const t=cell.ticket||{};
    if(t.length===292)full++;
    if(t.exhausted || t.cooldown_until>now)exh++;
    // Matches the cache hot path: exhausted does not invalidate a still-live token.
    if(t.length===292 && t.token && remaining(t,now)>0)live++;
  }));
  text($('st_total'),data.accounts?.length||0);text($('st_full'),full);text($('st_live'),live);text($('st_exh'),exh);
  setProgress('poolProgress',live,total);
  renderBreaker();
  if(['missing','expiring'].includes($('accountFilter').value))applyFilters();
}
function renderJob() {
  const job=data?.job;
  const active=activeJob(job);
  $('cancelJobBtn').disabled=!active || job.status==='cancelling';
  $('jobPanel').hidden=!job || (!active && job.id===dismissedJob);
  if(!job)return;
  $('dismissJobBtn').hidden=active;
  const states={running:'探测中',cancelling:'正在取消',done:'已结束',cancelled:'已取消'};
  text($('taskTitle'),states[job.status]||job.status);
  setProgress('taskProgress',Number(job.done)||0,Number(job.total)||0);
  $('taskProgressFill').classList.toggle('paused',job.status==='cancelled'||job.status==='cancelling');
  text($('jobMsg'),'已处理 '+job.done+'/'+job.total+' · 成功 '+(job.succeeded||0)+' · 耗尽 '+(job.exhausted||0)+' · 跳过 '+(job.skipped||0)+' · 取消 '+(job.cancelled_count||0));
  text($('jobTime'),job.finished_unix?'结束于 '+new Date(job.finished_unix*1000).toLocaleTimeString():'新格子可追加到本轮；处理完成不代表全部取票成功。');
}
function dismissJob() {
  if(!data?.job || activeJob(data.job))return;
  dismissedJob=data.job.id;
  try {sessionStorage.setItem('inject_dismissed_job',dismissedJob);}catch(e){}
  renderJob();
}
function reconcileOrder(parent,nodes) {
  let cursor=parent.firstElementChild;
  nodes.forEach(node=> {
    if(node===cursor)cursor=cursor.nextElementSibling;
    else parent.insertBefore(node,cursor);
  });
}
function render() {
  if(!data)return;
  const models=data.config?.models||[];
  const accounts=data.accounts||[];
  const ids=new Set(accounts.map(a=>a.id));
  accountViews.forEach((view,id)=> {if(!ids.has(id)){view.node.remove();accountViews.delete(id);}});
  const liveCells=new Map((data.job?.cells||[]).map(c=>[c.account_id+'\x00'+c.model.toLowerCase(),c]));
  accounts.forEach(a=> {
    let view=accountViews.get(a.id);
    if(!view){view=makeCard(a);accountViews.set(a.id,view);}
    view.account=a;
    renderPolicy(view);
    text(view.avatar,(a.email||'#').charAt(0).toUpperCase());text(view.mail,a.email||'#'+a.id);view.mail.title=a.email||'#'+a.id;
    text(view.meta,(a.plan_type||'未知套餐')+' · 账号 #'+a.id);
    view.select.setAttribute('aria-label','选择账号 '+(a.email||'#'+a.id));
    view.toggle.classList.toggle('on',!!a.harvest_enabled);view.toggle.setAttribute('aria-pressed',String(!!a.harvest_enabled));
    text(view.hint,a.harvest_enabled?'参与探测':'已暂停');
    const valid=new Set(models);
    view.models.forEach((cell,m)=>{if(!valid.has(m)){cell.node.remove();view.models.delete(m);}});
    models.forEach(m=> {
      let cell=view.models.get(m);if(!cell){cell=makeCell(a.id,m);view.models.set(m,cell);}
      const ticket=(a.tickets||[]).find(t=>t.model.toLowerCase()===m.toLowerCase())||{model:m};
      updateTicket(cell,ticket,liveCells.get(a.id+'\x00'+m.toLowerCase()));
    });
    reconcileOrder(view.cells,models.map(m=>view.models.get(m).node));
  });
  reconcileOrder($('accounts'),accounts.map(a=>accountViews.get(a.id).node));
  $('emptyTip').style.display=accounts.length?'none':'block';
  applyFilters();renderJob();tick();
}
function sanitizeSiteLogo(value) {
  if(typeof value!=='string')return '';
  const logo=value.trim(), lower=logo.toLowerCase();
  if(lower.startsWith('data:image/') && lower.includes(';base64,'))return logo;
  if(lower.startsWith('https://') || lower.startsWith('http://'))return logo;
  if(logo.startsWith('/') && !logo.startsWith('//'))return logo;
  return '';
}
function applySiteLogo(logo) {
  for(const id of ['siteFavicon','siteTouchIcon'])$(id).href=logo;
  document.querySelectorAll('.site-logo').forEach(img=> {
    img.onerror=logo==='/favicon.png'?null:()=>applySiteLogo('/favicon.png');
    img.src=logo;
  });
}
async function loadSiteBranding() {
  try {
    const response=await fetch('/api/branding',{credentials:'same-origin',signal:AbortSignal.timeout(5000)});
    if(!response.ok)return;
    const branding=await response.json();
    const name=typeof branding.site_name==='string'?branding.site_name.trim():'';
    document.title=(name||'CodexProxy')+' · Turn State Inject';
    const logo=sanitizeSiteLogo(branding.site_logo)||'/favicon.png';
    applySiteLogo(logo);
  } catch(e) { /* Keep the same built-in logo if branding cannot be loaded. */ }
}
if(window.matchMedia?.('(prefers-color-scheme: dark)').matches)document.documentElement.classList.add('dark');
void loadSiteBranding();
setColumns(columnsChoice);
configInputs.forEach(id=>{$(id).addEventListener('input',markCfgDirty);$(id).addEventListener('change',markCfgDirty);});
if(localStorage.getItem(KEY))load({fillCfg:true,initial:true});
setInterval(()=>{if(data && !document.hidden)tick();},1000);
setInterval(()=>{if($('app').style.display!=='none' && !document.hidden)load();},2000);
document.addEventListener('visibilitychange',()=>{if(!document.hidden && data)load();});
</script>
</body>
</html>
`)
