// Pure view logic shared with Node tests. No DOM, credentials, or device access.
(function (root) {
  function conversations(rows, peers, filter, query) {
    const grouped = new Map();
    for (const m of rows) {
      if (!grouped.has(m.peer)) grouped.set(m.peer, {peer: m.peer, latest: m, count: 0, unread: 0, ...peers[m.peer]});
      const c = grouped.get(m.peer);
      c.count++;
      if (m.direction === 'incoming' && !m.read) c.unread++;
      if (new Date(m.timestamp) > new Date(c.latest.timestamp)) c.latest = m;
    }
    const q = query.trim().toLocaleLowerCase();
    return [...grouped.values()].filter(c => {
      if (filter === 'archived' ? !c.archived : c.archived) return false;
      if (filter === 'unread' && !c.unread || filter === 'starred' && !c.starred) return false;
      return !q || (c.name || '').toLocaleLowerCase().includes(q) || c.peer.toLocaleLowerCase().includes(q) || rows.some(m => m.peer === c.peer && m.content.toLocaleLowerCase().includes(q));
    }).sort((a, b) => new Date(b.latest.timestamp) - new Date(a.latest.timestamp));
  }
  function unreadIDs(rows, peer) { return rows.filter(m => m.direction === 'incoming' && !m.read && (!peer || m.peer === peer)).map(m => m.id); }
  function statusLabel(s) { return {submitted: '已提交运营商', pending: '提交中', unknown: '结果未知，请先核对，勿重复发送', received: '已收到'}[s] || s; }
  function dayLabel(timestamp, now = new Date()) {
    const date = new Date(timestamp), today = new Date(now);
    if (date.toDateString() === today.toDateString()) return '今天';
    today.setDate(today.getDate() - 1);
    if (date.toDateString() === today.toDateString()) return '昨天';
    return date.toLocaleDateString('zh-CN', {year: 'numeric', month: 'long', day: 'numeric'});
  }
  function linePresentation(line, reachable = true, stale = false) {
    const state = !reachable ? 'service_offline' : stale ? 'stale' : line?.state || 'checking';
    const labels = {online:'线路在线',offline:'模块离线',no_sim:'未识别 SIM',sim_unknown:'SIM 状态未知',unregistered:'网络未注册',network_unknown:'网络状态未知',stale:'检测状态已过期',checking:'正在检测',service_offline:'NAS 无法连接'};
    const registration = line?.reg_status === 1 ? '已注册' : line?.reg_status === 5 ? '已注册（漫游）' : ({0:'未注册',2:'正在搜索网络',3:'注册被拒绝',4:'注册状态未知'}[line?.reg_status] || '状态未知');
    const fresh = reachable && !stale && !['stale','checking','service_offline'].includes(state);
    return {state,label:labels[state]||'状态未知',fresh,registration:fresh?registration:'待重新检测',module:fresh?(line?.module_connected?'已连接':'未连接'):'待重新检测',sim:fresh?({identified:'已识别',absent:'未识别',unknown:'状态未知'}[line?.sim_state]||'状态未知'):'待重新检测',tone:state==='online'?'online':['offline','service_offline'].includes(state)?'offline':['no_sim','unregistered'].includes(state)?'waiting':'unknown'};
  }
  const api = {conversations, unreadIDs, statusLabel, dayLabel, linePresentation};
  root.HomeSIMUI = api;
  if (typeof module !== 'undefined') module.exports = api;
})(globalThis);
