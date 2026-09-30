'use strict';
const $ = s => document.querySelector(s);
const UI = HomeSIMUI;
let rows = [], peers = {}, selected = '', filter = 'all', authenticated = false, setup = false;
let pc = null, mic = null, voiceID = '', callRows = [], installPrompt = null, loading = false, sendID = '';
let listKey = '', detailKey = '', detailLimit = 60, listPosition = 0, activeTab = 'inbox', markingRead = false, estimateRevision = 0, composeDirty = false;
let readTask = Promise.resolve();
let latestStatus = null, statusReceivedAt = 0, serviceFailed = false, revealedICCID = '', savingLine = false;
const text = (el, s) => { el.textContent = s; };
const format = t => new Date(t).toLocaleString('zh-CN', {month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false});
const clock = t => new Date(t).toLocaleTimeString('zh-CN', {hour:'2-digit',minute:'2-digit',hour12:false});
const element = (tag, className, value) => { const el = document.createElement(tag); if (className) el.className = className; if (value !== undefined) text(el, value); return el; };
function icon(name, className = 'icon') { const el = document.createElementNS('http://www.w3.org/2000/svg', 'svg'); el.setAttribute('class', className); el.setAttribute('aria-hidden','true'); const use = document.createElementNS('http://www.w3.org/2000/svg','use'); use.setAttribute('href','#i-'+name); el.append(use); return el; }
function button(label, className, fn, iconName) { const b = element('button',className); b.type='button'; b.setAttribute('aria-label',label); b.title=label; if(iconName)b.append(icon(iconName)); if(!className.includes('icon-button'))b.append(element('span','',label)); b.addEventListener('click',e=>{try{const result=fn(e);if(result?.catch)result.catch(()=>{})}catch(err){toast(err.message)}}); return b; }
function nameOf(peer) { return peers[peer]?.name || peer; }
function avatar(peer) { return element('span','avatar',peers[peer]?.name ? Array.from(peers[peer].name).slice(0,2).join('') : peer.slice(-2)); }
async function api(path, options = {}) {
  const controller = new AbortController(), timer = setTimeout(()=>controller.abort(), (!options.method || options.method==='GET')?10000:90000);
  let r,b; try { r = await fetch(path, {credentials:'same-origin',signal:controller.signal, ...options, headers:{'Content-Type':'application/json',...options.headers}}); b = await r.json(); } finally { clearTimeout(timer); }
  if (!r.ok) {
    if (r.status === 401 && path !== '/api/auth') { authenticated=false; $('#app').hidden=true; $('#auth').hidden=false; document.body.classList.remove('thread-open'); stopLocalAudio(); $('#compose').close(); $('#contact-dialog').close(); $('#line-dialog').close(); }
    throw Error(b.error || '请求失败');
  }
  return b;
}
const post = (path, data) => api(path,{method:'POST',body:JSON.stringify(data)});
function toast(message, label='', fn=null) {
  text($('#toast-text'),message); $('#toast').hidden=false;
  $('#toast-action').hidden=!label; text($('#toast-action'),label);
  $('#toast-action').onclick=fn ? async()=>{ $('#toast').hidden=true; try{await fn()}catch(e){toast(e.message)} } : null;
  clearTimeout(toast.timer); toast.timer=setTimeout(()=>$('#toast').hidden=true,label?9000:5000);
}
$('#toast-close').addEventListener('click',()=>$('#toast').hidden=true);
async function init() {
  try {
    const state=await api('/api/auth'); setup=state.setup_required;
    $('#setup-label').hidden=!setup; $('#setup-help').hidden=!setup;
    text($('#login-button'),setup?'创建自己的账户':'登录');
    $('#password').autocomplete=setup?'new-password':'current-password';
    if(state.authenticated)loggedIn();
  } catch { text($('#auth-error'),'服务暂时无法连接，请刷新重试。'); }
}
function loggedIn() { authenticated=true; $('#auth').hidden=true; $('#app').hidden=false; refresh(); if('serviceWorker' in navigator && isSecureContext)navigator.serviceWorker.register('/sw.js').catch(()=>{}); }
$('#login-form').addEventListener('submit',async e=>{
  e.preventDefault(); $('#login-button').disabled=true;
  try { await post('/api/auth',{username:$('#username').value,password:$('#password').value,setup_token:$('#setup-token').value.trim()}); $('#password').value=''; $('#setup-token').value=''; text($('#auth-error'),''); loggedIn(); }
  catch(e) {text($('#auth-error'),e.message)} finally {$('#login-button').disabled=false}
});
function switchTab(tab) {
  activeTab=tab;
  document.querySelectorAll('.panel').forEach(el=>el.hidden=el.id!==tab);
  document.querySelectorAll('[data-tab]').forEach(el=>{const on=el.dataset.tab===tab;el.classList.toggle('active',on);el.setAttribute('aria-current',on?'page':'false')});
  text($('#page-title'),{inbox:'短信',phone:'电话',settings:'我的设备'}[tab]);
  text($('#page-subtitle'),{inbox:'让每一条重要消息，都有归处。',phone:'把熟悉的声音，带到身边。',settings:'一条线路，连接自己的生活。'}[tab]);
  document.body.classList.toggle('thread-open',tab==='inbox'&&!!selected);
  if(tab==='phone')refreshCalls(); if(tab==='inbox')autoRead();
}
document.querySelectorAll('[data-tab]').forEach(b=>b.addEventListener('click',()=>switchTab(b.dataset.tab)));
function themeControls() { $('#theme-select').value=HomeTheme.get(); const dark=document.documentElement.dataset.theme==='dark'; $('#theme-toggle').replaceChildren(icon(dark?'sun':'moon')); $('#theme-toggle').setAttribute('aria-label',dark?'切换到浅色模式':'切换到深色模式'); }
$('#theme-toggle').addEventListener('click',()=>{HomeTheme.set(document.documentElement.dataset.theme==='dark'?'light':'dark');themeControls()});
$('#theme-select').addEventListener('change',()=>{HomeTheme.set($('#theme-select').value);themeControls()});
matchMedia('(prefers-color-scheme: dark)').addEventListener('change',themeControls); themeControls();
async function refresh() {
  if(!authenticated||loading)return; loading=true;
  try {
    const results=await Promise.allSettled([api('/api/status'),api('/api/messages'),api('/api/peers')]);
    if(!authenticated)return;
    const statusResult=results[0];
    if(statusResult.status==='fulfilled') {
      const status=statusResult.value;latestStatus=status;statusReceivedAt=Date.now();serviceFailed=false;
      text($('#operator'),status.operator||'SIM 线路');
      text($('#network'),status.connected?(status.network_mode||'蜂窝网络')+' · '+(status.reg_status_text||'等待注册'):'等待模块连接');
      text($('#firmware'),status.firmware||'—');text($('#audio-device'),status.audio_device||'—');text($('#secure-state'),status.https?'HTTPS 安全连接':'局域网 HTTP');
      text($('#sync-time'),clock(new Date()));$('#warning').hidden=!status.error;text($('#warning'),status.error||'');
      if(!isSecureContext)text($('#install-help'),'当前使用局域网 HTTP。配置受手机信任的 HTTPS 后，即可完整安装到主屏幕，并使用麦克风通话。短信功能现在可以使用。');
    } else {serviceFailed=true;}
    renderLineView();
    const dataOK=results[1].status==='fulfilled'&&results[2].status==='fulfilled';
    if(dataOK){rows=results[1].value;peers=results[2].value;renderList();renderDetail();renderContacts();autoRead()}
    $('#offline-warning').hidden=!serviceFailed&&dataOK;
    text($('#offline-warning'),serviceFailed?'暂时无法获取 NAS 状态，当前内容可能不是最新的。恢复连接后会自动同步。':'NAS 可以连接，但短信或备注同步失败；正在显示上次的内容。');
  } catch(e) { if(authenticated){serviceFailed=true;renderLineView();$('#offline-warning').hidden=false} }
  finally {loading=false}
}
function renderLineView() {
  const status=latestStatus||{},line=status.line||{},stale=statusReceivedAt>0&&Date.now()-statusReceivedAt>30000;
  const view=UI.linePresentation(line,!serviceFailed,stale);
  $('.line-card').dataset.state=view.state; text($('#connection'),view.label);$('#status-dot').className='dot '+view.tone;
  text($('#operator'),(status.operator||'SIM 线路')+(!view.fresh&&status.operator?' · 上次检测':''));
  text($('#network'),view.fresh&&line.module_connected?(status.network_mode||'蜂窝网络')+' · '+view.registration:'等待检测网络状态');
  text($('#service-state'),serviceFailed?'无法连接':stale?'状态已过期':statusReceivedAt?'在线':'检查中');
  text($('#module-state'),view.module);text($('#sim-state'),view.sim);text($('#registration-state'),view.registration);
  text($('#signal'),view.fresh&&line.module_connected&&status.signal_dbm&&status.signal_dbm>-150?status.signal_dbm+' dBm':'—');
  const number=view.fresh?line.number:'',source=number?({module:'模块读取',manual:'手动设置'}[line.number_source]||''):'';
  text($('#line-number'),number|| (view.state==='checking'?'手机号读取中':view.fresh&&line.sim_state==='identified'?'未读取到手机号':'手机号待检测'));
  text($('#number-source'),source);text($('#device-number'),number||'—');text($('#device-number-source'),source||'尚未获取');
  const iccid=view.fresh?line.iccid:'';if(iccid!==renderLineView.iccid){revealedICCID='';renderLineView.iccid=iccid}
  text($('#sim-iccid'),iccid?(revealedICCID===iccid?iccid:'•••• '+iccid.slice(-6)):'—');$('#reveal-iccid').hidden=!iccid;
  text($('#reveal-iccid'),revealedICCID===iccid?'隐藏完整卡号':'显示完整卡号');
  $('#edit-line-number').disabled=!view.fresh||!line.manual_allowed;
  text($('#edit-line-number'),line.manual_number?'修改备用号码':'设置备用号码');
  text($('#number-help'),!view.fresh?'等待最新模块状态，恢复连接后自动检测。':line.number_source==='module'?'号码来自 SIM 的本机号码记录；可能未由运营商核验。':line.number_source==='manual'?'当前显示你为这张 SIM 保存的备用号码。':line.number_detection==='query_failed'?'本机号码查询失败，将自动重试；也可以设置备用号码。':line.manual_allowed?'SIM 未返回本机号码，你可以设置备用号码。':'暂时无法确认 SIM 卡号，确认后才能绑定备用号码。');
  const checked=line.checked_at&&line.age_seconds>=0?format(line.checked_at):'—';text($('#line-check-time'),checked);
  if($('#line-dialog').open&&!savingLine){const changed=!view.fresh||!line.manual_allowed||$('#line-dialog').dataset.iccid!==iccid;$('#line-save').disabled=changed;if(changed)text($('#line-error'),'SIM 已变化或状态过期，请关闭后重新检测。')}
}
$('#reveal-iccid').addEventListener('click',()=>{const iccid=latestStatus?.line?.iccid||'';revealedICCID=revealedICCID===iccid?'':iccid;renderLineView()});
$('#edit-line-number').addEventListener('click',()=>{
  const line=latestStatus?.line;if(!line?.manual_allowed||serviceFailed)return;
  $('#line-dialog').dataset.iccid=line.iccid;text($('#line-card-hint'),'仅用于当前 SIM · 卡号末尾 '+line.iccid.slice(-6));
  $('#manual-number').value=line.manual_number||'';text($('#line-error'),'');$('#line-save').disabled=false;$('#line-dialog').showModal();$('#manual-number').focus();
});
$('#line-close').addEventListener('click',()=>$('#line-dialog').close());
$('#line-form').addEventListener('submit',async e=>{
  e.preventDefault();savingLine=true;$('#line-save').disabled=true;
  try{await post('/api/line/number',{iccid:$('#line-dialog').dataset.iccid,number:$('#manual-number').value.trim()});$('#line-dialog').close();toast('备用号码已保存');await refresh()}
  catch(e){text($('#line-error'),e.message)}finally{savingLine=false;$('#line-save').disabled=false;renderLineView()}
});
window.addEventListener('offline',()=>{serviceFailed=true;renderLineView()});window.addEventListener('online',refresh);
setInterval(()=>{if(authenticated&&!document.hidden)renderLineView()},5000);

function renderContacts() {
  const key=JSON.stringify(peers); if(renderContacts.key===key)return; renderContacts.key=key;
  const options=Object.entries(peers).filter(([,p])=>p.name).map(([number,p])=>{const o=element('option','',p.name);o.value=number;return o}); $('#contacts').replaceChildren(...options);
}
function emptyState(title,description,iconName='message') { const box=element('div','empty');const art=element('div','empty-art');art.append(icon(iconName));box.append(art,element('h3','',title),element('p','',description));return box; }
function renderList() {
  const query=$('#search').value;
  const items=UI.conversations(rows,peers,filter,query);
  const unread=rows.filter(m=>m.direction==='incoming'&&!m.read&&!peers[m.peer]?.archived).length;
  document.querySelectorAll('[data-unread]').forEach(el=>{text(el,unread>99?'99+':unread);el.hidden=!unread});
  text($('#unread-count'),unread>99?'99+':unread);$('#unread-count').hidden=!unread;
  text($('#count'),items.length); text($('#result-count'),`${items.length} 段会话${query?' · 搜索结果':''}`);
  $('#mark-all-read').hidden=!unread; $('#search-clear').hidden=!query;
  const titles={all:'收件箱',unread:'未读消息',starred:'收藏会话',archived:'归档会话'};
  $('#folder-title').firstChild.textContent=titles[filter]+' ';
  text($('#folder-description'),filter==='archived'?'归档只是收起，消息仍然在这里。':'留在家里的每一条消息。');
  const key=JSON.stringify([items,selected,filter,query,new Date().toDateString()]); if(key===listKey)return;listKey=key;
  const top=$('#messages').scrollTop;
  if(!items.length) {
    const titles=query?['没有找到相关消息','换个备注、号码或关键词再试试。']:filter==='unread'?['消息都看过了','新收到的未读短信，会出现在这里。']:filter==='starred'?['留一个位置给重要的人','打开会话，点击星标即可收藏。']:filter==='archived'?['归档里还没有消息','暂时不看的会话，可以先收在这里。']:['你的收件箱，准备好了','发到这张 SIM 的短信，会自动出现在这里。'];
    $('#messages').replaceChildren(emptyState(...titles,filter==='starred'?'star':filter==='archived'?'archive':'message'));return;
  }
  const fragment=document.createDocumentFragment();
  for(const c of items) {
    const b=element('button','message-row'+(selected===c.peer?' selected':'')+(c.unread?' unread':''));b.type='button';b.setAttribute('aria-label',`${nameOf(c.peer)}${c.unread?'，'+c.unread+' 条未读':''}，${c.latest.content}`);b.setAttribute('aria-pressed',String(selected===c.peer));
    const body=element('div','row-body'),top=element('div','row-top'),bottom=element('div','row-bottom');
    const date=new Date(c.latest.timestamp), today=new Date();const short=date.toDateString()===today.toDateString()?clock(date):date.toLocaleDateString('zh-CN',{month:'2-digit',day:'2-digit'});
    top.append(element('strong','',nameOf(c.peer)),element('time','',short));
    bottom.append(element('span','preview',(c.latest.direction==='outgoing'?'你：':'')+c.latest.content));
    if(c.starred)bottom.append(icon('star','icon mini-star'));
    if(c.unread)bottom.append(element('span','unread-badge',c.unread>99?'99+':c.unread));
    body.append(top,bottom);b.append(avatar(c.peer),body);b.addEventListener('click',()=>selectPeer(c.peer));fragment.append(b);
  }
  $('#messages').replaceChildren(fragment);$('#messages').scrollTop=top;
}
function selectPeer(peer) {
  listPosition=window.scrollY;selected=peer;detailLimit=60;detailKey='';$('#inbox').dataset.detail='true';document.body.classList.add('thread-open');
  renderList();renderDetail();if(innerWidth<=760)window.scrollTo(0,0);autoRead();
}
function backToList() { selected='';detailKey='';$('#inbox').dataset.detail='false';document.body.classList.remove('thread-open');renderList();renderDetail();if(innerWidth<=760)window.scrollTo(0,listPosition); }
function renderDetail() {
  if(!selected) {
    if(detailKey==='empty')return; detailKey='empty';const empty=emptyState('把消息留在这里','选择一段会话，继续联系。短信与备注，保存在自己的 NAS。');empty.append(button('写一条短信','quiet compact',()=>compose(),'edit'));$('#detail').replaceChildren(empty);return;
  }
  const all=rows.filter(m=>m.peer===selected).sort((a,b)=>new Date(a.timestamp)-new Date(b.timestamp));
  const messages=all.slice(-detailLimit),info=peers[selected]||{};
  const key=JSON.stringify([selected,messages,info,detailLimit,new Date().toDateString()]);if(key===detailKey)return;
  const old=$('.conversation-body'), oldTop=old?.scrollTop||0, oldHeight=old?.scrollHeight||0;
  const atBottom=!old||old.scrollHeight-old.scrollTop-old.clientHeight<55;
  const loadingOlder=renderDetail.older;renderDetail.older=false;detailKey=key;
  const head=element('div','detail-head'),person=element('div','detail-person'),title=element('div');
  person.append(button('返回会话列表','icon-button back-button',backToList,'back'),avatar(selected));title.append(element('h3','',nameOf(selected)),element('small','',info.name?selected:`${all.length} 条短信`));person.append(title);
  const tools=element('div','detail-tools');
  const star=button(info.starred?'取消收藏':'收藏会话','icon-button'+(info.starred?' is-starred':''),()=>changePeer({starred:!info.starred}),'star');star.setAttribute('aria-pressed',String(!!info.starred));
  tools.append(star,button('联系人备注','icon-button',openContact,'edit'),button('标记未读','icon-button',markThreadUnread,'message'),button(info.archived?'恢复会话':'归档会话','icon-button',()=>archiveThread(!info.archived),'archive'));
  head.append(person,tools);
  const body=element('div','conversation-body');body.setAttribute('aria-label','会话内容');
  if(all.length>detailLimit)body.append(button(`查看更早的消息（还有 ${all.length-detailLimit} 条）`,'quiet compact older-messages',()=>{detailLimit+=60;renderDetail.older=true;renderDetail()}));
  let lastDay='';
  for(const m of messages) {
    const day=UI.dayLabel(m.timestamp);if(day!==lastDay){body.append(element('div','date-divider',day));lastDay=day;}
    const bubble=element('div','bubble'+(m.direction==='outgoing'?' outgoing':''));bubble.append(element('span','',m.content));
    const timestamp=element('time',m.status==='unknown'?'unknown-status':'',clock(m.timestamp)+(m.direction==='outgoing'?' · '+UI.statusLabel(m.status):''));timestamp.dateTime=m.timestamp;bubble.append(timestamp);
    if(m.code){const code=element('button','code-button');code.type='button';code.setAttribute('aria-label','复制验证码 '+m.code);code.append(element('span','','验证码'),element('strong','',m.code),icon('copy'));code.addEventListener('click',()=>copyText(m.code,'验证码已复制'));bubble.append(code);}
    const actions=element('div','bubble-actions');actions.append(button('复制全文','text-button',()=>copyText(m.content,'短信已复制'),'copy'));bubble.append(actions);body.append(bubble);
  }
  if(!messages.length)body.append(emptyState('还没有消息','给这位联系人写一条短信吧。'));
  const actions=element('div','detail-actions');actions.append(element('small','',`${all.length} 条短信 · 保存在 NAS`),button('回复短信','primary compact',()=>compose(selected),'edit'));
  const parts=[head];
  if(info.archived){const note=element('div','archive-note','这段会话已归档，短信仍然保留。');note.append(button('恢复','text-button',()=>archiveThread(false)));parts.push(note)}
  $('#detail').replaceChildren(...parts,body,actions);
  if(loadingOlder)body.scrollTop=oldTop+body.scrollHeight-oldHeight;else body.scrollTop=atBottom?body.scrollHeight:oldTop;
}
async function changePeer(patch,peer=selected) {
  if(!peer)return;
  try {const info=await post('/api/peers',{peer,...patch});peers[peer]=info;renderList();renderDetail();renderContacts();return info;}
  catch(e){toast(e.message);throw e}
}
async function archiveThread(archived) {
  const peer=selected;if(!peer)return;
  try {await changePeer({archived},peer);backToList();toast(archived?'会话已归档，短信仍然保留':'会话已恢复到收件箱','撤销',async()=>{await changePeer({archived:!archived},peer)});}catch{}
}
function updateRead(ids,read) {
  readTask = readTask.catch(()=>{}).then(async()=>{
  for(let i=0;i<ids.length;i+=500) {
    const part=ids.slice(i,i+500);await post('/api/messages/read',{ids:part,read});
    const changed=new Set(part);rows.forEach(m=>{if(changed.has(m.id))m.read=read});
  }
  renderList();renderDetail();
  });
  return readTask;
}
async function autoRead() {
  if(markingRead||!selected||document.hidden||activeTab!=='inbox')return;
  const ids=UI.unreadIDs(rows,selected);if(!ids.length)return;markingRead=true;
  try{await updateRead(ids,true)}catch(e){toast('阅读状态未同步：'+e.message)}finally{markingRead=false}
}
async function markThreadUnread() {
  const peer=selected,ids=rows.filter(m=>m.peer===peer&&m.direction==='incoming').map(m=>m.id);
  if(!ids.length){toast('这段会话没有收到的短信');return}backToList();
  try{await updateRead(ids,false);toast('会话已标记未读')}catch(e){toast(e.message)}
}
$('#mark-all-read').addEventListener('click',async()=>{
  const ids=UI.unreadIDs(rows).filter(id=>!peers[rows.find(m=>m.id===id).peer]?.archived);$('#mark-all-read').disabled=true;
  try{if(ids.length)await updateRead(ids,true);toast('收件箱已全部标记为已读')}catch(e){toast(e.message)}finally{$('#mark-all-read').disabled=false}
});
function openContact() {text($('#contact-number'),selected);$('#contact-dialog').dataset.peer=selected;$('#contact-name').value=peers[selected]?.name||'';text($('#contact-error'),'');$('#contact-dialog').showModal();$('#contact-name').focus();}
$('#contact-close').addEventListener('click',()=>$('#contact-dialog').close());
$('#contact-form').addEventListener('submit',async e=>{e.preventDefault();$('#contact-save').disabled=true;try{await changePeer({name:$('#contact-name').value.trim()},$('#contact-dialog').dataset.peer);$('#contact-dialog').close();toast('联系人备注已保存')}catch(e){text($('#contact-error'),e.message)}finally{$('#contact-save').disabled=false}});
async function copyText(s,message='已复制') {
  try {await navigator.clipboard.writeText(s);toast(message);return}catch{}
  // LAN HTTP does not expose Clipboard API. A user-clicked selection can still
  // be copied by browsers supporting the legacy command; otherwise keep it selected.
  const field=element('textarea');field.value=s;field.className='copy-field';document.body.append(field);field.focus();field.select();
  let copied=false;try{copied=document.execCommand('copy')}catch{}field.remove();toast(copied?message:'当前浏览器无法复制，请长按短信内容复制');
}
$('#search').addEventListener('input',renderList);$('#search-clear').addEventListener('click',()=>{$('#search').value='';renderList();$('#search').focus()});
document.querySelectorAll('[data-filter]').forEach(b=>b.addEventListener('click',()=>{filter=b.dataset.filter;document.querySelectorAll('[data-filter]').forEach(el=>{const on=el===b;el.classList.toggle('active',on);el.setAttribute('aria-pressed',String(on))});backToList();}));
$('#refresh').addEventListener('click',async()=>{$('#refresh').disabled=true;try{await post('/api/messages/refresh',{});await refresh();toast('同步请求已提交，新消息会自动更新');setTimeout(refresh,1500)}catch(e){toast(e.message)}finally{$('#refresh').disabled=false}});
function compose(peer='') {
  if(composeDirty&&$('#sms-content').value&& !confirm('已有未发送内容，是否放弃并重新编写？'))return;
  ++estimateRevision;clearTimeout(compose.estimateTimer);
  sendID=globalThis.crypto?.randomUUID?.()||Date.now().toString(16)+Math.random().toString(16).slice(2);
  $('#recipient').value=peer;$('#sms-content').value='';text($('#send-error'),'');text($('#sms-length'),'0 字 · 0 段');$('#sms-length').classList.remove('multi-part');composeDirty=false;
  $('#compose').showModal();(peer?$('#sms-content'):$('#recipient')).focus();
}
$('#compose-open').addEventListener('click',()=>compose());$('#empty-compose').addEventListener('click',()=>compose());
function closeCompose(){if(composeDirty&&!confirm('短信还没有发送，确定放弃这次编写吗？'))return;composeDirty=false;$('#compose').close()}
$('#compose-close').addEventListener('click',closeCompose);$('#compose').addEventListener('cancel',e=>{e.preventDefault();closeCompose()});
$('#sms-content').addEventListener('input',()=>{
  composeDirty=!!$('#sms-content').value;const value=$('#sms-content').value,revision=++estimateRevision;clearTimeout(compose.estimateTimer);text($('#sms-length'),`${Array.from(value).length} 字 · 计算段数…`);
  compose.estimateTimer=setTimeout(async()=>{try{const b=await post('/api/messages/estimate',{message:value});if(revision!==estimateRevision)return;text($('#sms-length'),`${b.characters} 字 · ${b.segments} 段`);$('#sms-length').classList.toggle('multi-part',b.segments>1)}catch(e){if(revision===estimateRevision)text($('#sms-length'),`${Array.from(value).length} 字 · ${e.message}`)}},300);
});
$('#send-form').addEventListener('submit',async e=>{
  e.preventDefault();$('#send-button').disabled=true;
  try {const m=await post('/api/messages/send',{phone:$('#recipient').value.trim(),message:$('#sms-content').value,request_id:sendID});if(m.status!=='submitted')throw Error('发送结果尚未确认，请先核对记录，不要重复发送');composeDirty=false;$('#compose').close();toast('短信已提交运营商');await refresh();filter='all';document.querySelectorAll('[data-filter]').forEach(b=>{const on=b.dataset.filter==='all';b.classList.toggle('active',on);b.setAttribute('aria-pressed',String(on))});selectPeer(m.peer);}
  catch(e){text($('#send-error'),e.message);await refresh()}finally{$('#send-button').disabled=false}
});
for(const k of '123456789*0#'){const b=document.createElement('button');text(b,k);b.addEventListener('click',async()=>{if(callRows.length===1&&callRows[0].state===0){try{await action('dtmf',{digit:k})}catch(e){toast(e.message)}}else{$('#dial-number').value+=k}});$('#keypad').append(b)}
function stopLocalAudio(){if(pc){pc.onconnectionstatechange=null;pc.close();pc=null}if(mic){mic.getTracks().forEach(t=>t.stop());mic=null}voiceID='';$('#disconnect-audio').hidden=true;text($('#audio-state'),'音频未连接')}
async function connectAudio(){if(!isSecureContext)throw Error('麦克风通话需要通过 HTTPS 打开家信');if(pc&&pc.connectionState==='connected')return;if(pc)stopLocalAudio();mic=await navigator.mediaDevices.getUserMedia({audio:{echoCancellation:true,noiseSuppression:true},video:false});pc=new RTCPeerConnection({iceServers:[]});const audio=new Audio();audio.autoplay=true;pc.ontrack=e=>{audio.srcObject=e.streams[0]||new MediaStream([e.track]);audio.play().catch(()=>toast('请点一下页面启用声音'))};const sender=pc.addTrack(mic.getAudioTracks()[0],mic);const transceiver=pc.getTransceivers().find(t=>t.sender===sender);const codecs=RTCRtpSender.getCapabilities('audio').codecs.filter(c=>c.mimeType.toLowerCase()==='audio/pcmu');if(!codecs.length)throw Error('浏览器不支持此模块的语音格式');if(transceiver.setCodecPreferences)transceiver.setCodecPreferences(codecs);pc.onconnectionstatechange=()=>{const state=pc?.connectionState;text($('#audio-state'),state==='connected'?'麦克风已连接，可以拨打或接听。':'正在连接音频…');if(state==='failed'||state==='disconnected'){stopLocalAudio();api('/api/voice',{method:'DELETE'}).catch(()=>{});toast('音频连接已断开，请重新连接')}};const offer=await pc.createOffer();await pc.setLocalDescription(offer);await new Promise((resolve,reject)=>{const timer=setTimeout(()=>reject(Error('语音候选地址收集超时')),12000);if(pc.iceGatheringState==='complete'){clearTimeout(timer);resolve();return}pc.onicegatheringstatechange=()=>{if(pc?.iceGatheringState==='complete'){clearTimeout(timer);resolve()}}});const answer=await post('/api/voice/offer',pc.localDescription);voiceID=answer.voice_id;await pc.setRemoteDescription({type:answer.type,sdp:answer.sdp});$('#disconnect-audio').hidden=false}
$('#connect-audio').addEventListener('click',async()=>{try{await connectAudio()}catch(e){stopLocalAudio();toast(e.message)}});$('#disconnect-audio').addEventListener('click',async()=>{try{await api('/api/voice',{method:'DELETE'});stopLocalAudio()}catch(e){stopLocalAudio();toast(e.message)}});
async function action(action,extra={}){return post('/api/calls/action',{action,voice_id:voiceID,call_id:callRows[0]?.id||'',...extra})}
$('#dial').addEventListener('click',async()=>{const b=$('#dial');b.disabled=true;try{await action('dial',{number:$('#dial-number').value.trim()});toast('正在拨号');await refreshCalls()}catch(e){toast(e.message)}finally{b.disabled=false}});
for(const name of ['answer','reject','hangup'])$('#'+name).addEventListener('click',async()=>{try{await action(name);await refreshCalls()}catch(e){toast(e.message)}});
async function refreshCalls(){if(!authenticated||$('#phone').hidden)return;try{callRows=await api('/api/calls');const c=callRows[0];text($('#call-number'),c?(c.number||'未知号码'):'线路空闲');text($('#call-state'),c?({0:'通话中',1:'通话保持',2:'正在拨号',3:'等待对方接听',4:'有来电',5:'有等待接听的来电'}[c.state]||'状态未知'):'电话音频需要 HTTPS，以及浏览器麦克风权限。');const ring=c&&(c.state===4||c.state===5);$('#answer').hidden=!ring;$('#reject').hidden=!ring;$('#hangup').hidden=!c||ring;}catch(e){text($('#call-state'),e.message)}}
$('#logout').addEventListener('click',async()=>{try{if(pc)await api('/api/voice',{method:'DELETE'});stopLocalAudio();await api('/api/auth',{method:'DELETE'});composeDirty=false;location.reload()}catch(e){toast(e.message)}});
window.addEventListener('beforeinstallprompt',e=>{e.preventDefault();installPrompt=e;$('#install').hidden=false});$('#install').addEventListener('click',async()=>{if(installPrompt){await installPrompt.prompt();installPrompt=null;$('#install').hidden=true}});
document.addEventListener('visibilitychange',()=>{if(!document.hidden){refresh();refreshCalls();autoRead()}});setInterval(()=>{if(!document.hidden)refresh()},12000);setInterval(()=>{if(!document.hidden)refreshCalls()},3000);init();

window.addEventListener('beforeunload',e=>{if(composeDirty){e.preventDefault();e.returnValue=''}});
