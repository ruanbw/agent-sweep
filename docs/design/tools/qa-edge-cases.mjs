import { launchBrowser } from './shoot.mjs';
const base='file://' + process.cwd() + '/docs/design/prototype/index.html';
const R=[]; const add=(n,p,d='')=>R.push({n,p,d});
const b=await launchBrowser(); const p=await b.newPage({width:1440,height:900}); let out;
const open=async(q)=>{await p.open(base+'?theme=light&dock=0&'+q,{waitMs:500});};
try{
  // scan
  await open('state=idle');
  const before=await p.evaljs("document.querySelectorAll('#sessionBody tr.session-row').length");
  await p.click('#scanBtn');
  const running=await p.evaljs("({status:document.getElementById('scanStatus').innerText, label:document.getElementById('scanBtnLabel')?.innerText, progress:!!document.querySelector('#scanStatus [role=progressbar]'), rows:document.querySelectorAll('#sessionBody tr.session-row').length})");
  add('扫描中：进度/按钮/列表保留', running.status.includes('扫描中') && running.progress && running.rows===before, JSON.stringify(running));
  await p.waitForFunction("document.getElementById('scanStatus').innerText.includes('扫描完成')",{timeout:5000});
  add('扫描完成 toast + 状态', await p.evaljs("document.getElementById('toasts').innerText.includes('扫描完成') && document.getElementById('scanBtnLabel').innerText==='扫描'"));
  // error banner
  await open('state=error');
  add('部分失败 banner 显示 2 个 Agent', await p.evaljs("!document.getElementById('issueBanner').hidden && document.getElementById('issueBannerText').innerText.includes('2 个')"));
  await p.click('#issueBannerBtn');
  add('banner 展开逐 Agent 明细 + 重试', await p.evaljs("!!document.querySelector('.issue-list') && document.querySelectorAll('.issue-row').length===2 && !!document.querySelector('[data-retry]')"));
  await p.click('[data-retry]');
  await new Promise(r=>setTimeout(r,1200));
  add('重试后 issue 减少', await p.evaljs("document.querySelectorAll('.issue-row').length===1"));
  // filters age/size
  await open('state=idle');
  await p.click('#filterBtn'); await p.waitFor('#filterPopover'); await p.click('[data-filter="age"][data-value="older"]');
  add('筛选「更早」= 5 条', await p.evaljs("document.querySelectorAll('#sessionBody tr.session-row').length===5"));
  await p.click('[data-filter="age"][data-value="all"]'); await p.click('[data-filter="size"][data-value="large"]');
  add('筛选「大于 1 MB」= 2 条', await p.evaljs("document.querySelectorAll('#sessionBody tr.session-row').length===2"));
  await p.click('[data-filter="size"][data-value="all"]'); await p.key('Escape');
  // quick empty/largest
  await p.click('#quickSelectBtn'); await p.waitFor('#quickPopover'); await p.click('[data-quick="empty"]');
  add('快速选择空会话 = 2 条', await p.evaljs("document.querySelectorAll('tr[aria-selected=\"true\"]').length===2"));
  await p.click('#quickSelectBtn'); await p.click('[data-quick="largest"]');
  add('快速选择最大 10 条 = 10 条', await p.evaljs("document.querySelectorAll('tr[aria-selected=\"true\"]').length===10"));
  await p.click('#quickSelectBtn'); await p.click('[data-quick="clear"]');
  add('快速选择清除 = 0 条', await p.evaljs("document.querySelectorAll('tr[aria-selected=\"true\"]').length===0"));
  // keyboard nav + enter + detail toggle
  await p.key('j'); await p.key('j');
  add('J 移动焦点且不选择', await p.evaljs("document.querySelectorAll('tr.session-row.is-focused').length===1 && document.querySelectorAll('tr[aria-selected=\"true\"]').length===0"));
  await p.click('#detailToggle');
  add('详情开关收起', await p.evaljs("document.getElementById('app').dataset.detail==='closed'"));
  await p.click('#detailToggle');
  add('详情开关展开', await p.evaljs("document.getElementById('app').dataset.detail==='open'"));
  // search no results
  await p.fill('#searchInput','zzz-不存在');
  add('无结果空态 + 清除入口', await p.evaljs("document.querySelectorAll('#sessionBody tr.session-row').length===0 && !document.getElementById('emptyState').hidden && document.getElementById('emptyAction').innerText.includes('清除')"));
  await p.click('#emptyAction');
  add('清除搜索恢复 26 条', await p.evaljs("document.querySelectorAll('#sessionBody tr.session-row').length===26"));
  // Ctrl? Cmd+Z undo after a clean
  await p.click('.row-check',{index:0}); await p.click('.row-check',{index:1});
  await p.click('#cleanBtn'); await p.waitFor('#reviewDialog'); await p.click('#reviewConfirm');
  await p.waitForFunction("document.querySelectorAll('#sessionBody tr.session-row').length===24",{timeout:5000});
  await p.key('z',{meta:true});
  await p.waitForFunction("document.querySelectorAll('#sessionBody tr.session-row').length===26",{timeout:5000});
  add('⌘Z 撤销最近清理', await p.evaljs("document.querySelectorAll('#sessionBody tr.session-row').length===26"));
  // prefs toggles
  await p.click('#settingsBtn'); await p.waitFor('#settingsSheet');
  await p.click('#prefConfirm'); await p.click('#prefSnapshots'); await p.click('#prefAutoScan');
  add('设置开关可切换', await p.evaljs("!document.getElementById('prefConfirm').checked && !document.getElementById('prefSnapshots').checked && !document.getElementById('prefAutoScan').checked"));
  await p.key('Escape');
  out={R, errors:p.uniqueErrors(), warnings:p.consoleWarnings, external:p.externalRequests};
}catch(err){out={fatal:String(err.stack||err),R,errors:p.uniqueErrors?.(),warnings:p.consoleWarnings,external:p.externalRequests};}
finally{await p.close();await b.close()}
console.log(JSON.stringify(out,null,2));
