const {test}=require('node:test');const assert=require('node:assert/strict');const UI=require('./ui.js');
const rows=[{id:'a',peer:'10086',content:'older',direction:'incoming',read:false,timestamp:'2026-09-30T01:00:00Z'},{id:'b',peer:'10086',content:'latest',direction:'outgoing',read:true,timestamp:'2026-09-30T02:00:00Z'},{id:'c',peer:'13800000000',content:'code 123456',direction:'incoming',read:true,timestamp:'2026-09-30T03:00:00Z'},{id:'d',peer:'12345',content:'archived',direction:'incoming',read:false,timestamp:'2026-09-30T04:00:00Z'}];
const peers={'10086':{name:'移动',starred:true},'12345':{archived:true}};
test('grouping finds latest even with unordered input and counts unread',()=>{const c=UI.conversations(rows,peers,'all','');assert.equal(c.length,2);assert.equal(c[1].latest.id,'b');assert.equal(c[1].unread,1)});
test('filters retain archived messages and search whole conversations or aliases',()=>{assert.deepEqual(UI.conversations(rows,peers,'archived','').map(c=>c.peer),['12345']);assert.deepEqual(UI.conversations(rows,peers,'unread','').map(c=>c.peer),['10086']);assert.equal(UI.conversations(rows,peers,'starred','移动')[0].peer,'10086');assert.equal(UI.conversations(rows,peers,'all','older')[0].latest.id,'b');assert.equal(UI.conversations(rows,peers,'all','missing').length,0)});
test('read snapshot never includes outgoing or another sender',()=>{assert.deepEqual(UI.unreadIDs(rows,'10086'),['a'])});
test('dates and uncertain send status are presented honestly',()=>{assert.equal(UI.dayLabel('2026-09-30T12:00:00',new Date('2026-09-30T13:00:00')),'今天');assert.equal(UI.dayLabel('2026-09-29T12:00:00',new Date('2026-09-30T13:00:00')),'昨天');assert.match(UI.statusLabel('unknown'),/勿重复发送/)});
test('line status distinguishes service, modem, SIM and registration',()=>{
 const online={state:'online',module_connected:true,sim_state:'identified',reg_status:5};
 assert.equal(UI.linePresentation(online).label,'线路在线');assert.equal(UI.linePresentation(online).registration,'已注册（漫游）');
 assert.equal(UI.linePresentation(online,false).label,'NAS 无法连接');assert.equal(UI.linePresentation(online,false).fresh,false);
 assert.equal(UI.linePresentation(online,true,true).label,'检测状态已过期');assert.equal(UI.linePresentation(online,true,true).fresh,false);
 assert.equal(UI.linePresentation({...online,state:'no_sim',sim_state:'absent'}).sim,'未识别');
 assert.equal(UI.linePresentation({...online,state:'offline',module_connected:false}).module,'未连接');
 assert.equal(UI.linePresentation({...online,state:'unregistered',reg_status:2}).registration,'正在搜索网络');
});
