import { launchBrowser } from './shoot.mjs';
const base='file://' + process.cwd() + '/docs/design/prototype/index.html';
const R=[]; const add=(n,p,d='')=>R.push({n,p,d});
const b=await launchBrowser(); const p=await b.newPage({width:1440,height:900}); let out;
const open=async(q)=>{await p.open(base+'?theme=light&dock=0&'+q,{waitMs:520});};
const e=(s)=>p.evaljs(s);
try{
  await open('state=idle');
  add('初始 26 条会话 / 0 选择 / 清理禁用', await e("document.querySelectorAll('#sessionBody tr.session-row').length===26 && document.querySelectorAll('tr[aria-selected=\"true\"]').length===0 && document.getElementById('cleanBtn').disabled"));
  await p.click('#sessionBody tr.session-row',{index:3});
  add('点击行只更新焦点与详情（不勾选）', await e("document.querySelectorAll('tr[aria-selected=\"true\"]').length===0 && document.querySelectorAll('tr.session-row.is-focused').length===1 && !!document.querySelector('.detail-title')"));
  await p.click('.row-check',{index:0}); await p.click('.row-check',{index:2});
  add('勾选 2 条 + 状态栏/按钮同步', await e("document.querySelectorAll('tr[aria-selected=\"true\"]').length===2 && /已选\\s*2\\s*条/.test(document.getElementById('selectionSummary').innerText) && document.getElementById('cleanBtn').innerText.includes('清理 2 条')"));
  await p.click('#quickSelectBtn'); await p.waitFor('#quickPopover'); await p.click('[data-quick="old"]');
  add('快速选择是替换（超过 30 天 = 5 条）', await e("document.querySelectorAll('tr[aria-selected=\"true\"]').length===5"));
  await p.key('Escape'); await p.click('.row-check',{index:0}); await p.click('.row-check',{index:4,shift:true});
  add('Shift 连选区间 = 5 条', await e("document.querySelectorAll('tr[aria-selected=\"true\"]').length===5"));
  await p.click('.row-check',{index:6});
  await p.key('a',{meta:true});
  add('复选框聚焦时 ⌘A 全选 26 条', await e("document.querySelectorAll('tr[aria-selected=\"true\"]').length===26"));
  await p.key('Escape');

  await p.fill('#searchInput','sqlite');
  add('搜索命中且计数正确', await e("document.querySelectorAll('#sessionBody tr.session-row').length===1 && document.getElementById('listMeta').innerText.includes('1 条')"));
  await p.key('Escape');
  add('Esc 清空搜索并恢复 26 条', await e("document.getElementById('searchInput').value==='' && document.querySelectorAll('#sessionBody tr.session-row').length===26"));
  await p.click('#filterBtn'); await p.waitFor('#filterPopover'); await p.click('[data-filter="storage"][data-value="sqlite"]');
  add('筛选 SQLite = 3 条 + chip', await e("document.querySelectorAll('#sessionBody tr.session-row').length===3 && document.getElementById('filterChips').innerText.includes('SQLite')"));
  await p.click('[data-filter="storage"][data-value="all"]'); await p.key('Escape');
  await p.click('[data-sort="bytes"]');
  add('按体积降序首行为 2.1 MB', await e("document.querySelector('#sessionBody tr.session-row .cell-size').innerText.includes('2.1 MB')"));

  // review / delete / undo on fresh load
  await open('state=idle');
  for (let i=0;i<3;i++) await p.click('.row-check',{index:i});
  await p.click('#cleanBtn'); await p.waitFor('#reviewDialog');
  add('确认框：3 条标题 + 3 行账本 + 总计', await e("document.getElementById('reviewTitle').innerText.includes('3 条') && document.querySelectorAll('#reviewLedger .ledger-row').length===3 && !!document.querySelector('#reviewLedger .ledger-total')"));
  add('账本含「已备份」且无编造体积', await e("document.getElementById('reviewLedger').innerText.includes('已备份') && !/\\d+\\s*KB\\s*\\n?保留在应用支持目录/.test(document.getElementById('reviewLedger').innerText)"));
  add('活动会话警告点名 Claude Code', await e("!document.getElementById('reviewActive').hidden && document.getElementById('reviewActive').innerText.includes('Claude Code')"));
  await p.click('#reviewModeSeg input[value="permanent"]');
  add('永久删除：警告 + 确认勾选 + 禁用', await e("!document.getElementById('reviewPermanentWarn').hidden && !document.getElementById('reviewAckRow').hidden && document.getElementById('reviewConfirm').disabled"));
  await p.click('#reviewAck');
  add('勾选后确认可用', await e("!document.getElementById('reviewConfirm').disabled"));
  await p.click('#reviewModeSeg input[value="trash"]');
  await p.click('#reviewConfirm');
  await p.waitForFunction("document.querySelectorAll('#toasts .toast').length && document.getElementById('toasts').innerText.includes('已移入废纸篓 3 条')",{timeout:5000});
  add('删除 3 条 + toast 撤销 + 批次回执', await e("document.querySelectorAll('#sessionBody tr.session-row').length===23 && !document.getElementById('undoChip').hidden && document.getElementById('undoChip').innerText.includes('撤销')"));
  await e("document.querySelector('#toasts .toast-action')?.click()");
  await p.waitForFunction("document.getElementById('toasts').innerText.includes('已恢复 3 条会话')",{timeout:5000});
  add('撤销恢复 26 条 + 回执', await e("document.querySelectorAll('#sessionBody tr.session-row').length===26"));

  // discover
  await open('state=idle');
  await p.click('#discoverBtn');
  await p.waitForFunction("document.querySelectorAll('#discoverList .candidate-card').length===2",{timeout:5000});
  add('发现抽屉：2 候选 + 收录 CTA', await e("document.querySelectorAll('#discoverList .candidate-card').length===2 && document.querySelector('[data-adopt]').innerText.includes('收录')"));
  await p.click('[data-adopt]',{index:0});
  add('收录后侧栏 8→9 行 + 已收录 + toast', await e("document.querySelectorAll('#agentGroups .agent-row').length===9 && !!document.querySelector('.candidate-card.is-adopted') && document.getElementById('toasts').innerText.includes('已收录')"));
  await p.key('Escape');

  // settings with a selection so clean button label updates
  await open('state=idle'); await p.click('.row-check',{index:0});
  await p.click('#settingsBtn'); await p.waitFor('#settingsSheet');
  await p.click('input[name="appearance"][value="dark"]');
  add('设置：外观即时切换深色', await e("document.documentElement.dataset.theme==='dark'"));
  await p.click('input[name="deleteMode"][value="permanent"]');
  add('设置：永久模式同步状态栏按钮', await e("document.getElementById('cleanBtn').innerText.includes('永久删除')"));
  const scroll=await e("(()=>{const b=document.getElementById('batchList'),body=document.querySelector('#settingsSheet .sheet-body');return {items:b.children.length, scrollable:body.scrollHeight>body.clientHeight, overflow:getComputedStyle(body).overflowY};})()");
  add('设置：最近清理可滚动（最后一条需滚动可见）', scroll.scrollable && scroll.overflow==='auto', JSON.stringify(scroll));
  await p.key('Escape');
  await p.click('#settingsBtn'); await p.waitFor('#settingsSheet'); await p.click('input[name="deleteMode"][value="trash"]'); await p.key('Escape');
  await p.click('#densityBtn');
  add('密度切换为紧凑', await e("document.getElementById('prototype').dataset.density==='compact'"));

  // palette / context / shortcuts
  await open('state=idle');
  await p.key('k',{meta:true}); await p.waitFor('#palette');
  const pal0=await e("[...document.querySelectorAll('#palette .palette-row')].findIndex(x=>x.classList.contains('is-active'))");
  await p.key('ArrowDown');
  const pal1=await e("[...document.querySelectorAll('#palette .palette-row')].findIndex(x=>x.classList.contains('is-active'))");
  add('命令面板打开/↑↓/分组', pal0===0 && pal1===1 && await e("document.querySelectorAll('#palette .palette-group').length>=2"));
  await p.key('Escape');
  await p.click('#sessionBody tr.session-row',{index:2,button:'right'});
  await new Promise(r=>setTimeout(r,180));
  add('右键菜单锚定行内且 4 项', await e("(()=>{const m=document.getElementById('contextMenu'),r=m.getBoundingClientRect(),row=document.querySelector('#sessionBody tr.session-row').getBoundingClientRect();return !m.hidden && m.querySelectorAll('.menu-item').length===4 && r.x>0 && r.y>0;})()"));
  await p.key('Escape');
  await p.key('?');
  add('? 打开快捷键表', await e("!document.getElementById('shortcutsDialog').hidden && document.querySelectorAll('#shortcutsList .shortcut-row').length>=10"));
  await p.key('Escape');
  await p.click('[data-path-action="copy"]');
  await p.waitForFunction("document.querySelectorAll('#toasts .toast').length>0",{timeout:3000});
  add('复制路径有 toast 兜底', await e("document.getElementById('toasts').innerText.includes('已复制路径')"));
  await new Promise(r=>setTimeout(r,6700));
  add('toast 约 6s 自动消失', await e("document.querySelectorAll('#toasts .toast').length===0"));

  // a11y
  add('aria：行/Agent/sort/live/progressbar 语义', await e("(()=>{const row=document.querySelector('tr.session-row'),agent=document.querySelector('.agent-row'),th=document.querySelector('th[aria-sort]');return row.hasAttribute('aria-selected') && agent.hasAttribute('aria-pressed') && !!th && document.getElementById('toasts').getAttribute('aria-live')==='polite' && !!document.querySelector('[role=progressbar]');})()"));
  add('reduced-motion 媒体查询存在', await e("[...document.styleSheets].some(s=>{try{return [...s.cssRules].some(r=>String(r.conditionText||'').includes('prefers-reduced-motion'))}catch(e){return false}})"));

  // responsive
  for (const [w,h,n] of [[1440,900,'1440'],[1180,760,'1180'],[960,700,'960']]) {
    await p.setViewport(w,h); await open('state=idle');
    const info=await p.inspect();
    add('响应式 '+n+' 无横向滚动', info.horizontalScroll.document===0 && info.horizontalScroll.window===0, JSON.stringify(info.horizontalScroll));
  }
  await p.setViewport(1440,900);
  out={R, errors:p.uniqueErrors(), warnings:p.consoleWarnings, external:p.externalRequests};
}catch(err){out={fatal:String(err.stack||err),R,errors:p.uniqueErrors?.(),warnings:p.consoleWarnings,external:p.externalRequests};}
finally{await p.close();await b.close()}
console.log(JSON.stringify(out,null,2));
