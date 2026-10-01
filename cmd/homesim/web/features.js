'use strict';
const features = {devices:[], loading:false};
let featureBusy=0;
function featureButton(label,style,fn,iconName){return button(label,style,async e=>{featureBusy++;try{return await fn(e)}catch(error){toast(error.message)}finally{featureBusy--}},iconName)}
function featureSection(title, description, open=false) {
  const section=element('details','feature-section');section.open=open;
  const summary=element('summary'),copy=element('div');copy.append(element('h3','',title),element('p','muted',description));summary.append(copy,icon('plus'));
  const content=element('div','feature-content');section.append(summary,content);return {section,content};
}
function field(form,label,name,type='text',value='') {
  const wrap=element('label','',label),input=element(type==='select'?'select':'input');input.name=name;
  if(type!=='select')input.type=type;input.value=value;input.autocomplete='off';
  if(type==='checkbox'){wrap.className='check-label';input.checked=!!value;wrap.prepend(input)}else wrap.append(input);
  form.append(wrap);return input;
}
function selectOptions(input,items) {input.replaceChildren(...items.map(([value,label])=>{const o=element('option','',label);o.value=value;return o}));}
function submitForm(form,label,task) {const b=featureButton(label,'primary compact',()=>{});b.type='submit';const result=element('p','muted');result.setAttribute('role','status');form.append(b,result);form.addEventListener('submit',async e=>{e.preventDefault();b.disabled=true;featureBusy++;text(result,'正在处理…');try{await task(result);if(result.textContent==='正在处理…')text(result,'已保存')}catch(e){text(result,e.message)}finally{b.disabled=false;featureBusy--}});return result;}
const pickerWrap=element('label','device-picker','当前线路'),devicePicker=element('select');devicePicker.setAttribute('aria-label','当前 SIM 线路');pickerWrap.append(devicePicker);$('header').after(pickerWrap);
devicePicker.addEventListener('change',async()=>{
  const next=devicePicker.value;
  if(featureBusy||dialing||connectingAudio||savingLine||$('#send-button').disabled||$('#contact-save').disabled||pc||nativeState.ready||nativeState.active||callRows.length){devicePicker.value=selectedDevice;toast('请先结束当前操作、挂断并断开音频，再切换线路');return}
  if(composeDirty&&!confirm('短信还没有发送，切换线路会放弃这次编写，确定继续吗？')){devicePicker.value=selectedDevice;return}
  composeDirty=false;$('#compose').close();$('#contact-dialog').close();$('#line-dialog').close();stopLocalAudio();
  selectedDevice=next;localStorage.setItem('homesim.device',next);rows=[];peers={};selected='';listKey='';detailKey='';callRows=[];latestStatus=null;statusReceivedAt=0;callsReceivedAt=0;renderList();renderDetail();renderLineView();
  if(window.HomeSIMNative)try{await nativeRequest('session')}catch(e){toast(e.message)}
  await refresh();await refreshFeatures();await refreshCalls();
});
const devicesBox=featureSection('我的线路','每个模块单独保存短信、通话记录与音频设置。',true);
const deviceStats=element('div','device-stats'),deviceCards=element('div','device-grid');devicesBox.content.append(deviceStats,deviceCards);
$('#settings').prepend(devicesBox.section);
const historyBox=featureSection('通话记录','来电、未接和拨出记录保存在 NAS。回拨只填入号码。',true),historyList=element('div','call-history');historyBox.content.append(historyList);$('#phone').append(historyBox.section);
async function refreshFeatures() {
  if(!authenticated||features.loading)return;features.loading=true;const chosen=selectedDevice;
  try {
    const [devices,history]=await Promise.allSettled([api('/api/devices'),api('/api/calls/history')]);
    if(devices.status==='fulfilled'){
      features.devices=devices.value;const exists=features.devices.some(d=>d.config.id===selectedDevice);
      const options=features.devices.map(d=>[d.config.id,d.config.name+' · '+(d.line.state==='online'?'在线':d.line.state==='offline'?'离线':'检测中')]);if(!exists)options.unshift([selectedDevice,'所选线路暂不可用']);selectOptions(devicePicker,options);devicePicker.value=selectedDevice;
      const online=features.devices.filter(d=>d.line.state==='online').length;deviceStats.replaceChildren(element('span','',features.devices.length+' 条线路'),element('span','',online+' 条在线'),element('span','',features.devices.length-online+' 条待检测 / 离线'));
      deviceCards.replaceChildren(...features.devices.map(d=>deviceCard(d)));
    }
    if(history.status==='fulfilled'&&chosen===selectedDevice)renderHistory(history.value);
    document.querySelectorAll('.export-actions a').forEach(link=>{const u=new URL(link.href);u.searchParams.set('device_id',selectedDevice);link.href=u.href});
  }catch(e){toast(e.message)}finally{features.loading=false}
}
window.refreshFeatures=refreshFeatures;
function deviceCard({config,line}) {
  const card=element('article','device-card'),state=UI.linePresentation(line,true,false);
  card.append(element('span','badge',state.label),element('h3','',config.name),element('p','',line.number||'号码尚未获取'),element('small','muted',config.port));
  const tools=element('div','feature-actions');tools.append(button(config.id===selectedDevice?'当前线路':'使用这条线路','quiet compact',()=>{devicePicker.value=config.id;devicePicker.dispatchEvent(new Event('change'))}),featureButton('线路设置','text-button',()=>editDevice(config)));
  card.append(tools);return card;
}
function featureDialog(title) {
  const dialog=element('dialog','feature-dialog'),head=element('div','dialog-heading');head.append(element('h2','',title),featureButton('关闭','quiet compact',()=>dialog.close()));dialog.append(head);document.body.append(dialog);dialog.addEventListener('close',()=>dialog.remove());return dialog;
}
async function editDevice(config=null) {
  const ports=await api('/api/devices/discover'),dialog=featureDialog(config?'线路设置':'添加线路'),form=element('form');dialog.append(form);
  const name=field(form,'线路名称','name','text',config?.name||'新线路');name.maxLength=40;
  const port=field(form,'AT 串口','port','select');selectOptions(port,[...new Set([config?.port,...ports].filter(Boolean))].map(p=>[p,p]));if(config)port.value=config.port;port.disabled=!!config;
  const audio=field(form,'音频设备','audio','text',config?.audio||'disabled');audio.placeholder='hw:Baiwang,0 或 disabled';
  const enabled=field(form,'启用这条线路','enabled','checkbox',config?.enabled??true);
  form.append(element('p','muted','同一个 USB 模块的多个串口只选 AT 口。多模块请分别选择对应的声卡；不能共用同一音频设备。'));
  submitForm(form,'保存线路',async()=>{await post('/api/devices',{id:config?.id||'manual-'+crypto.randomUUID(),name:name.value.trim(),port:port.value,audio:audio.value.trim(),enabled:enabled.checked});dialog.close();await refreshFeatures()});dialog.showModal();
}
devicesBox.content.append(featureButton('添加线路','quiet compact',()=>editDevice(), 'plus'),element('p','muted','已知 Baiwang 模块会自动发现。第二个模块需要 Docker 允许 USB 热插拔；其他型号可手动指定 AT 串口。'));
function renderHistory(rows) {
  if(!rows.length){historyList.replaceChildren(element('p','muted','还没有通话记录。NAS 从升级后开始记录，关闭页面也会继续。'));return}
  historyList.replaceChildren(...rows.slice(0,100).map(row=>{
    const item=element('div','history-row'),copy=element('div'),labels={completed:'已接通',missed:'未接来电',outgoing:'拨出未接通',ongoing:'进行中',interrupted:'服务重启，结果未确认'};
    copy.append(element('strong','',row.number||'模块未提供号码'),element('small','muted',(row.direction===1?'来电':'拨出')+' · '+(labels[row.result]||'结果未知')+' · '+format(row.started)));
    if(row.answered&&row.ended)copy.append(element('small','muted','约 '+Math.max(0,Math.round((new Date(row.ended)-new Date(row.answered))/1000))+' 秒'));
    item.append(copy);if(row.number)item.append(featureButton('回拨','quiet compact',()=>{$('#dial-number').value=row.number;switchTab('phone');updatePhoneControls();$('#dial-number').focus()}));return item;
  }));
}
const netBox=featureSection('网络与 SIM 设置','查询 APN、IMS 注册状态，或向运营商发送 USSD。');$('#settings').append(netBox.section);
const netResult=element('p','muted','选择当前线路后点击检测。'),networkForm=element('form','feature-form');
const apn=field(networkForm,'APN','apn'),mode=field(networkForm,'运营商选择','mode','select');selectOptions(mode,[['automatic','自动选择'],['manual','手动选择']]);
const plmn=field(networkForm,'运营商代码（手动模式）','plmn');plmn.placeholder='例如 46000';let networkICCID='';
netBox.content.append(featureButton('检测网络设置','quiet compact',async()=>{const result=await api('/api/network');networkICCID=result.iccid;apn.value=result.apn||result.saved?.apn||'';plmn.value=result.saved?.plmn||'';mode.value=result.saved?.mode||'automatic';text(netResult,(result.apn_known?'APN 已读取':'APN 无法读取')+' · '+(result.ims_known?'IMS 状态：'+result.ims:'IMS 状态未确认')+' · '+(result.card_verified?'卡号已确认':'卡号未确认，暂不能保存'))}),netResult,networkForm);
submitForm(networkForm,'应用并按卡保存',async result=>{if(!networkICCID)throw Error('请先检测当前 SIM');if(!confirm('应用网络设置可能使这条 SIM 暂时重新注册网络，确定继续吗？')){text(result,'已取消');return}await post('/api/network',{iccid:networkICCID,apn:apn.value.trim(),mode:mode.value,plmn:plmn.value.trim()});text(result,'已应用。设置与 SIM 卡号绑定，后续可重新检测。')});
const ussdForm=element('form','feature-form'),ussdInput=field(ussdForm,'USSD 指令','command');ussdInput.placeholder='例如 *100#';submitForm(ussdForm,'发送 USSD',async result=>{if(!confirm('发送此 USSD 指令给运营商？部分指令可能办理套餐或产生费用。')){text(result,'已取消');return}const response=await post('/api/network/ussd',{command:ussdInput.value.trim()});text(result,response.Text||response.text||JSON.stringify(response))});netBox.content.append(ussdForm);
const notifyBox=featureSection('通知提醒','新短信、来电和未接来电。默认隐藏内容，密钥仅保存在 NAS。');$('#settings').append(notifyBox.section);
const notifyForm=element('form','feature-form'),channel=field(notifyForm,'通知渠道','channel','select');selectOptions(channel,[['bark','Bark'],['telegram','Telegram'],['smtp','邮件（SMTP 587 / STARTTLS）'],['webhook','Webhook']]);
const notifyURL=field(notifyForm,'Bark / Webhook HTTPS 地址','url','url'),notifySecret=field(notifyForm,'Telegram Token / SMTP 密码','secret','password'),notifyTarget=field(notifyForm,'Telegram Chat ID / 收件邮箱','target'),notifyUser=field(notifyForm,'发件邮箱','username','email'),notifyHost=field(notifyForm,'SMTP 主机','host');
const notifyContent=field(notifyForm,'通知中包含短信正文与来电号码','include_content','checkbox',false),notifyEnabled=field(notifyForm,'开启通知（保存后自动发送后续事件）','enabled','checkbox',false);
function notifyFields(){notifyURL.parentElement.hidden=!['bark','webhook'].includes(channel.value);notifySecret.parentElement.hidden=!['telegram','smtp'].includes(channel.value);notifyTarget.parentElement.hidden=!['telegram','smtp'].includes(channel.value);notifyUser.parentElement.hidden=channel.value!=='smtp';notifyHost.parentElement.hidden=channel.value!=='smtp'}channel.addEventListener('change',notifyFields);notifyFields();
notifyBox.content.append(featureButton('读取通知设置','quiet compact',async()=>{const response=await api('/api/notifications'),c=response.config;channel.value=c.channel||'bark';notifyEnabled.checked=c.enabled;notifyContent.checked=c.include_content;notifyTarget.value=c.target||'';notifyUser.value=c.username||'';notifyHost.value=c.host||'';notifyURL.value='';notifySecret.value='';notifyURL.placeholder=response.has_url?'已有地址，留空保留':'https://';notifySecret.placeholder=response.has_secret?'已有密钥，留空保留':'';notifyFields()}),notifyForm);
submitForm(notifyForm,'保存通知设置',async()=>{await post('/api/notifications',{channel:channel.value,enabled:notifyEnabled.checked,include_content:notifyContent.checked,url:notifyURL.value.trim(),secret:notifySecret.value,target:notifyTarget.value.trim(),username:notifyUser.value.trim(),host:notifyHost.value.trim()});notifySecret.value='';notifyURL.value=''});
if(window.HomeSIMNative) {const nativeBox=featureSection('iPhone 系统电话','CallKit 使用原生音频；锁屏来电需要 NAS 配置 Apple APNs。'),result=element('p','muted');nativeBox.content.append(featureButton('开启锁屏来电','primary compact',async()=>{const s=await nativeRequest('enablePush');text(result,s.status)}),featureButton('关闭锁屏来电','quiet compact',async()=>{await nativeRequest('disablePush');text(result,'锁屏来电已关闭')}),result);$('#settings').append(nativeBox.section)}
const diagBox=featureSection('诊断与日志','记录模块、网络、音频和通知事件；日志不包含短信或密码。'),diagState=element('p','muted'),eventList=element('div','event-list');diagBox.content.append(featureButton('检查当前线路','quiet compact',async()=>{const d=await api('/api/diagnostics');text(diagState,'音频：'+(d.audio_connected?'已连接':'未连接')+' · ICE：'+d.ice_state+' · TURN：'+(d.turn_configured?'已配置':'未配置')+' · 模块→手机 '+d.captured_frames+' 帧 / 手机→模块 '+d.played_frames+' 帧（传输计数，声音仍需试听）');eventList.replaceChildren(...d.events.slice().reverse().map(e=>element('p','muted',format(e.at)+' · '+e.message)));if(!d.events.length)eventList.append(element('p','muted','暂无诊断事件'))}),diagState,eventList);$('#settings').append(diagBox.section);
const esimBox=featureSection('eSIM 管理','需要支持 APDU 的模块和兼容 eSIM 卡。检测会读取卡片，不下载套餐。'),esimState=element('p','muted'),profileList=element('div','device-grid');esimBox.content.append(featureButton('检测 eSIM 芯片','quiet compact',async()=>{const result=await jobRequest('/api/esim/inspect',{},esimState);renderProfiles(result)}),esimState,profileList);$('#settings').append(esimBox.section);
async function jobRequest(path,data,status) {
  const device=selectedDevice,job=await post(path,data);text(status,'正在处理，请勿重复提交。');
  for(let i=0;i<150;i++){await new Promise(r=>setTimeout(r,2000));const state=await api('/api/jobs?id='+encodeURIComponent(job.id)+'&device_id='+encodeURIComponent(device));text(status,state.step+' · '+state.progress+'%');if(state.state==='failed')throw Error(state.error);if(state.state==='completed'){text(status,'操作已完成');return state.result}}
  throw Error('操作仍可能在 NAS 上进行，请稍后重新检测，不要重复提交。');
}
function renderProfiles(overview) {
  const groups=overview?.profiles||[];profileList.replaceChildren();if(!groups.length){text(esimState,'未发现兼容的 eSIM 芯片。普通实体 SIM 无法仅靠软件变成 eSIM。');return}
  for(const group of groups){const card=element('article','device-card');card.append(element('h3','',overview.chip_info?.sku_name||'eSIM 芯片'),element('p','muted','EID · '+group.eid));
    for(const profile of group.profiles){const row=element('div','profile-row');row.append(element('strong','',profile.name||profile.service_provider_name||'未命名套餐'),element('p','muted',profile.iccid+' · '+profile.state_text));
      const invoke=async action=>{if(!confirm(({enable:'切换后当前套餐将停用。确认启用这个套餐？',disable:'停用后本卡可能无法接收短信和电话，确认？',delete:'删除套餐通常无法恢复。确认永久删除？'})[action]||'确认修改套餐？'))return;const name=action==='rename'?prompt('套餐名称',profile.name||''):'';if(action==='rename'&&name===null)return;await jobRequest('/api/esim/action',{action,iccid:profile.iccid,aid:group.aid_hex,eid:group.eid,name:name||''},esimState);const result=await jobRequest('/api/esim/inspect',{},esimState);renderProfiles(result)};
      row.append(featureButton(profile.state===1?'停用':'启用','quiet compact',()=>invoke(profile.state===1?'disable':'enable')),featureButton('重命名','text-button',()=>invoke('rename')),featureButton('删除','danger compact',()=>invoke('delete')));card.append(row)}
    const download=element('form','feature-form'),smdp=field(download,'SM-DP+ 地址','smdp'),matching=field(download,'Matching ID','matching_id'),confirmation=field(download,'确认码（可选）','confirmation','password');submitForm(download,'下载套餐',async result=>{if(!confirm('激活码可能只能使用一次。确认下载到这张 eSIM 卡？')){text(result,'已取消');return}await jobRequest('/api/esim/action',{action:'download',aid:group.aid_hex,eid:group.eid,smdp:smdp.value.trim(),matching_id:matching.value.trim(),confirmation:confirmation.value},result);confirmation.value='';matching.value=''});card.append(download);profileList.append(card)}
}
const proxyBox=featureSection('代理与流量','HTTP / SOCKS5 代理仅使用当前模块的 USB 网口；流量统计只计算代理传输。'),proxyState=element('p','muted'),proxyForm=element('form','feature-form'),proxyChart=element('div','traffic-chart');
const proxyMode=field(proxyForm,'代理协议','mode','select');selectOptions(proxyMode,[['http','HTTP / CONNECT'],['socks5','SOCKS5（TCP）']]);const proxyIface=field(proxyForm,'本模块网络接口','interface','select'),proxyListen=field(proxyForm,'NAS 局域网监听 IP','listen','text','127.0.0.1'),proxyPort=field(proxyForm,'监听端口','port','number','1080'),proxyUsername=field(proxyForm,'用户名','username'),proxyPassword=field(proxyForm,'密码（至少 12 字符）','password','password'),proxyEnabled=field(proxyForm,'启用代理','enabled','checkbox',false);
async function loadProxy(){const d=await api('/api/proxy'),c=d.config;selectOptions(proxyIface,(d.interfaces||[]).map(n=>[n,n]));proxyIface.value=c.interface||d.interfaces[0]||'';proxyMode.value=c.mode||'http';proxyListen.value=c.listen||'127.0.0.1';proxyPort.value=c.port||1080;proxyUsername.value=c.username||'';proxyEnabled.checked=c.enabled;proxyPassword.value='';proxyPassword.placeholder=d.password_set?'留空保留已保存密码':'';text(proxyState,(!d.interfaces.length?'未检测到本模块 USB 网口。模块需先建立数据连接。':d.running?'代理正在运行':'代理尚未运行')+(d.error?' · '+d.error:'')+' · 本次服务上传 '+bytes(d.upload)+' / 下载 '+bytes(d.download));renderTraffic(d.history||[])}
function bytes(n){if(!n)return '0 B';const units=['B','KB','MB','GB'];const i=Math.min(3,Math.floor(Math.log(n)/Math.log(1024)));return (n/1024**i).toFixed(i?1:0)+' '+units[i]}
const trafficPeriod=element('select');trafficPeriod.setAttribute('aria-label','流量统计周期');selectOptions(trafficPeriod,[['day','今天'],['week','近七天'],['month','近三十天']]);proxyBox.content.append(trafficPeriod);let trafficPoints=[];trafficPeriod.addEventListener('change',()=>renderTraffic(trafficPoints));
function renderTraffic(points){trafficPoints=points;const days={day:1,week:7,month:30}[trafficPeriod.value];const since=new Date();if(days===1)since.setHours(0,0,0,0);else since.setTime(Date.now()-days*86400000);const data=points.filter(p=>new Date(p.at)>=since),groups=new Map();for(const p of data){const t=new Date(p.at);t.setMinutes(0,0,0);if(days>1)t.setHours(0);const key=t.toISOString(),g=groups.get(key)||{at:key,upload:0,download:0};g.upload+=p.upload;g.download+=p.download;groups.set(key,g)}const rows=[...groups.values()];const total=rows.reduce((t,p)=>({upload:t.upload+p.upload,download:t.download+p.download}),{upload:0,download:0});if(!rows.length){proxyChart.replaceChildren(element('p','muted','此周期暂无代理流量采样。开启后每五分钟保存一次。'));text(trafficTotal,'');trafficTable.replaceChildren();return}const max=Math.max(1,...rows.map(p=>p.upload+p.download));proxyChart.replaceChildren(...rows.map(p=>{const bar=element('div','traffic-bar');bar.style.setProperty('--value',Math.max(2,(p.upload+p.download)/max*100)+'%');bar.title=format(p.at)+' · 上传 '+bytes(p.upload)+' · 下载 '+bytes(p.download);bar.setAttribute('role','img');bar.setAttribute('aria-label',bar.title);return bar}));text(trafficTotal,'周期上传 '+bytes(total.upload)+' · 下载 '+bytes(total.download));trafficTable.replaceChildren(...rows.reverse().map(p=>{const row=element('div','history-row');row.append(element('span','muted',format(p.at)),element('span','muted','↑ '+bytes(p.upload)+' / ↓ '+bytes(p.download)));return row}))}

proxyBox.content.append(featureButton('检测网口与代理状态','quiet compact',loadProxy),proxyState,proxyChart,proxyForm);submitForm(proxyForm,'保存代理设置',async()=>{await post('/api/proxy',{enabled:proxyEnabled.checked,mode:proxyMode.value,listen:proxyListen.value.trim(),port:Number(proxyPort.value),interface:proxyIface.value,username:proxyUsername.value.trim(),password:proxyPassword.value});proxyPassword.value='';await loadProxy()});$('#settings').append(proxyBox.section);
setInterval(()=>{if(!document.hidden)refreshFeatures()},12000);
if(authenticated)refreshFeatures();

const preferences=featureSection('线路详情与偏好','号码、线路状态、外观、导出与账户。');const legacyGrid=$('#settings .settings-grid');if(legacyGrid){preferences.content.append(legacyGrid);$('#settings>.section-heading')?.remove();$('#settings').append(preferences.section)}

const trafficTotal=element('p','muted'),trafficTable=element('div','event-list');proxyChart.after(trafficTotal,trafficTable);
