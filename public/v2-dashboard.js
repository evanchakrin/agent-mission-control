/* Isolated candidate integration. Legacy boot never loads this module. */
(function (root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.AMCV2Dashboard = api;
})(typeof globalThis === 'object' ? globalThis : this, function () {
  'use strict';
  const escape = s => String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  function preserveViewPosition(pane){
    const top=pane.scrollTop,left=pane.scrollLeft,tableLeft=pane.querySelector?.('.table-wrap')?.scrollLeft;
    const expanded=new Set(Array.from(pane.querySelectorAll?.('details[id]')||[]).filter(el=>el.open).map(el=>el.id));
    return ()=>{for(const el of pane.querySelectorAll?.('details[id]')||[])el.open=expanded.has(el.id);pane.scrollTop=top;pane.scrollLeft=left;const table=pane.querySelector?.('.table-wrap');if(table&&tableLeft!=null)table.scrollLeft=tableLeft;};
  }
  function briefPricing(totals){
    if(!totals)return 'Pricing';
    const p=totals.currentComparisons;
    if(!p)return 'Usage and pricing';
    const coverage=p.recordedTokens>0?Math.round(p.pricedTokens/p.recordedTokens*100)+'% token coverage':'no recorded usage';
    return `API estimate: ${p.costEstimate==null?'unpriced':'~$'+Number(p.costEstimate).toLocaleString(undefined,{minimumFractionDigits:2,maximumFractionDigits:2})} · ${coverage} · not a bill`;
  }

  class RunAlerts {
    constructor(read,notify,{hidden=()=>false}={}){this.read=read;this.notify=notify;this.hidden=hidden;this.seen=new Map();this.epoch=null;}
    async poll(machines){
      if(this.closed||this.pending||this.hidden())return;this.pending=true;
      try{
        const page=await this.read();if(this.closed||this.hidden())return;
        if(!page||!Array.isArray(page.runs)||page.runs.length>100||typeof page.recoveryEpoch!=='string')throw Error('Invalid run activity');
        const connected=new Set((machines||[]).filter(m=>m.connection==='connected').map(m=>m.machine.heartbeat.machineId));
        const next=new Map(),alerts=[];const sameEpoch=this.epoch===page.recoveryEpoch;
        for(const r of page.runs){
          if(typeof r.id!=='string'||next.has(r.id)||typeof r.active!=='boolean'||typeof r.synced!=='boolean'||typeof r.revision!=='string'||!Number.isFinite(Date.parse(r.lastActivity))||r.stalled!==null&&typeof r.stalled!=='boolean')throw Error('Invalid run');
          const previous=sameEpoch?this.seen.get(r.id):null;
          const reliable=r.synced&&connected.has(r.machineId);
          if(reliable&&previous?.reliable&&previous.revision===r.revision){
            if(r.stalled===true&&previous.stalled===false)alerts.push(['error','Run stalled: a recorded tool call has had no result for over 2 minutes.',r]);
            if(previous.active&&!r.active)alerts.push(['done','Run finished (inferred from 10 minutes without chat activity; not a success verdict).',r]);
          }
          next.set(r.id,{...r,reliable});
        }
        this.seen=next;this.epoch=page.recoveryEpoch;
        for(const [kind,message,r] of alerts)this.notify(kind,message,{id:'',name:r.title||'Session '+r.id.slice(0,12),sessionId:r.id});
      }catch{/* Retry next cycle; an unavailable snapshot is not a finished run. */}
      finally{this.pending=false;}
    }
    close(){this.closed=true;this.seen.clear();}
  }

  class ReleaseAlerts {
    constructor(read,notify,{hidden=()=>false,now=()=>Date.now()}={}){this.read=read;this.notify=notify;this.hidden=hidden;this.now=now;this.next=0;this.seen=new Set();}
    async poll(){
      if(this.closed||this.pending||this.hidden()||this.now()<this.next)return;
      this.pending=true;this.next=this.now()+15*60*1000;
      try{
        const value=await this.read();
        if(this.closed||this.hidden()||value?.state!=='ready')return;
        if(typeof value.current!=='string'||value.current.length>128||typeof value.latest!=='string'||!/^\d+\.\d+\.\d+$/.test(value.latest))return;
        this.next=this.now()+6*60*60*1000;this.latest=value.latest;
        if(value.updateAvailable===true&&!this.seen.has(value.latest)){
          this.seen.add(value.latest);while(this.seen.size>20)this.seen.delete(this.seen.values().next().value);
          this.notify('done',`AMC ${value.latest} is available (hub ${value.current}). Review the release before updating; nothing was installed automatically.`,{id:'',name:'AMC release update'});
        }
      }catch{/* An unavailable release server does not mean AMC is unhealthy. */}
      finally{this.pending=false;}
    }
    close(){this.closed=true;this.seen.clear();}
    observeMachines(rows){
      if(this.closed||this.hidden()||!this.latest)return;
      const target=this.latest.split('.').map(Number);
      for(const row of rows||[]){
        const h=row?.machine?.heartbeat;if(row.connection!=='connected'||!h?.machineId)continue;
        const key='machine:'+h.machineId+':'+this.latest;if(this.seen.has(key))continue;
        const version=h.version,parts=typeof version==='string'&&/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-[\w.-]+)?$/.test(version)?version.split('-')[0].split('.').map(Number):null;
        let older=!version;
        if(parts){for(let i=0;i<3;i++){if(parts[i]!==target[i]){older=parts[i]<target[i];break}}}
        if(!older)continue;
        this.seen.add(key);while(this.seen.size>1000)this.seen.delete(this.seen.values().next().value);
        this.notify('error',`Satellite ${h.name||h.machineId} reports ${version||'an unknown version'}; AMC ${this.latest} is available. Update that satellite when convenient; nothing was installed automatically.`,{id:h.machineId,name:h.name||h.machineId});
      }
    }
  }

  class SessionAlerts {
    constructor(client,notify,{hidden=()=>false}={}){this.client=client;this.notify=notify;this.hidden=hidden;this.seen=new Map();this.cursor=null;this.epoch='';}
    async poll(){
      if(this.closed||this.pending||this.hidden())return;
      this.pending=true;
      try{
        const head=await this.client.changeHead();
        if(this.closed||this.hidden())return;
        const epoch=head.hubId+':'+head.recoveryEpoch;
        if(this.cursor===null||epoch!==this.epoch||head.sequence<this.cursor){this.cursor=head.sequence;this.epoch=epoch;this.seen.clear();return;}
        if(head.sequence===this.cursor)return;
        const changes=await this.client.changes({after:this.cursor,limit:20});
        const next=new Map(this.seen),alerts=[],checked=new Set();let cursor=this.cursor;
        for(const change of changes){
          if(this.closed||this.hidden())return;
          const id=change.sessionId;
          if(change.kind==='projection'){next.delete(id);cursor=change.sequence;continue;}
          if(change.kind!=='session'||!id||checked.has(id)){cursor=change.sequence;continue;}
          if(checked.size===10)break;
          const stats=await this.client.stats(id);
          if(stats?.sessionId!==id||!Number.isSafeInteger(stats.errors)||stats.errors<0)throw Error('Invalid session statistics');
          const previous=next.get(id);
          if(stats.errors>0&&(previous===undefined||stats.errors>previous))alerts.push({id,count:stats.errors,previous});
          next.delete(id);next.set(id,stats.errors);while(next.size>1000)next.delete(next.keys().next().value);
          checked.add(id);cursor=change.sequence;
        }
        const end=await this.client.changeHead();
        if(this.closed||this.hidden())return;
        if(end.hubId+':'+end.recoveryEpoch!==epoch||end.sequence<cursor){this.cursor=null;this.seen.clear();return;}
        this.seen=next;this.cursor=cursor;
        for(const alert of alerts)this.notify('error',alert.previous===undefined?`Session has ${alert.count} recorded tool errors. First observed by this dashboard; errors may be historical.`:`Recorded tool-error count increased by ${alert.count-alert.previous} (now ${alert.count}). This is indexed evidence, not proof the run failed.`,{id:'',name:'Session '+alert.id.slice(0,12),sessionId:alert.id});
        this.failed=false;
      }catch{
        if(!this.closed&&!this.hidden()&&!this.failed){this.failed=true;this.notify('error','Session-error checks are unavailable; the last completed change position is retained.',{id:'',name:'AMC session alerts'});}
      }finally{this.pending=false;}
    }
    close(){this.closed=true;this.seen.clear();}
  }

  class BudgetAlerts {
    constructor(read,budget,notify,{now=()=>new Date(),hidden=()=>false}={}) {
      this.read=read;this.budget=budget;this.notify=notify;this.now=now;this.hidden=hidden;this.lastCheck=0;
      this.message='Checks run while this dashboard is visible.';
    }
    async poll(force=false) {
      if(this.closed||this.pending||this.hidden())return;
      const now=this.now();if(!force&&now.getTime()-this.lastCheck<300000)return;
      let budget;try{budget=Number(this.budget());}catch{this.message='Budget settings are unavailable.';return;}
      if(!Number.isFinite(budget)||budget<=0){this.message='Budget alerts are off.';return;}
      const start=new Date(now.getFullYear(),now.getMonth(),now.getDate());
      const end=new Date(now.getFullYear(),now.getMonth(),now.getDate()+1),day=start.toISOString();
      this.pending=true;this.lastCheck=now.getTime();
      try {
        const page=await this.read({from:day,to:end.toISOString(),limit:100});
        if(this.closed||this.hidden()||Number(this.budget())!==budget)return;
        const finished=this.now();
        if(new Date(finished.getFullYear(),finished.getMonth(),finished.getDate()).toISOString()!==day){this.lastCheck=0;return;}
        if(!Array.isArray(page?.groups)||page.groups.length>100||page.nextCursor)throw Error('Incomplete totals');
        let cost=0,priced=0,unpriced=0;const keys=new Set();
        for(const row of page.groups) {
          const counts=[row.tokensIn,row.tokensCache,row.tokensCacheWrite,row.tokensOut,row.pricedTokens,row.unpricedTokens];
          if(typeof row.key!=='string'||keys.has(row.key)||!counts.every(n=>Number.isSafeInteger(n)&&n>=0))throw Error('Invalid totals');
          keys.add(row.key);const recorded=counts.slice(0,4).reduce((a,b)=>a+b,0);
          if(!Number.isSafeInteger(recorded)||row.pricedTokens+row.unpricedTokens!==recorded)throw Error('Invalid coverage');
          if(row.pricedTokens>0&&(!Number.isFinite(row.costEstimate)||row.costEstimate<0))throw Error('Invalid estimate');
          cost+=row.pricedTokens>0?row.costEstimate:0;priced+=row.pricedTokens;unpriced+=row.unpricedTokens;
        }
        if(!Number.isFinite(cost)||!Number.isSafeInteger(priced)||!Number.isSafeInteger(unpriced))throw Error('Totals overflow');
        this.message=priced?`Today’s directly attributed estimates: ~$${cost.toFixed(3)}; ${unpriced} unpriced tokens.`:'Today’s cost is unavailable: no priced usage observations.';
        if(cost>budget&&this.notifiedDay!==day){
          this.notifiedDay=day;
          this.notify('error',`Today’s directly attributed estimates (~$${cost.toFixed(3)}) exceed your $${budget.toFixed(2)} budget. ${unpriced} tokens remain unpriced. Recorded usage may contain duplicates; this is not an invoice.`,{id:'',name:'Daily budget'});
        }
      } catch {if(!this.closed)this.message='Budget check unavailable; missing costs are not zero.';}
      finally{this.pending=false;}
    }
    close(){this.closed=true;}
  }

  class CollectorAlerts {
    constructor(read, notify, {hidden=()=>false, schedule=(fn,ms)=>setTimeout(fn,ms), cancel=id=>clearTimeout(id),afterPoll=()=>{}}={}) {
      this.read=read;this.notify=notify;this.hidden=hidden;this.schedule=schedule;this.cancel=cancel;
      this.seen=new Map();this.closed=false;this.pending=false;this.failed=false;this.afterPoll=afterPoll;
    }
    async poll() {
      if(this.closed||this.pending||this.hidden())return;
      this.cancel(this.timer);this.pending=true;
      try {
        const rows=await this.read();
        if(this.closed||this.hidden())return;
        if(!Array.isArray(rows)||rows.length>1000)throw new Error('Invalid collector status');
        const next=new Map();
        for(const row of rows) {
          const h=row?.machine?.heartbeat,id=h?.machineId,state=row?.connection;
          if(typeof id!=='string'||!id||next.has(id)||!['connected','delayed','disconnected'].includes(state))throw new Error('Invalid collector status');
          const before=this.seen.get(id);
          const stages={collection:h.collectionState||h.state,upload:h.connectionState};
          const machine={state,name:row.machine.label?.displayName||h.name||id,id};
          for(const stage of ['collection','upload']){
            const value=stages[stage],ready=stage==='collection'?'ready':'connected';
            machine[stage]=state==='connected'&&(value===ready||['blocked_storage','blocked_auth','blocked_conflict'].includes(value))?value:before?.[stage];
          }
          next.set(id,machine);
        }
        if(this.failed)this.notify('done','Hub connection restored; collector status is available again.',{id:'',name:'AMC hub'});
        this.failed=false;
        for(const [id,machine] of next) {
          const before=this.seen.get(id);
          for(const stage of ['collection','upload']){
            const value=machine[stage],prior=before?.[stage];
            if(!value||value===prior||machine.state!=='connected')continue;
            if(value.startsWith('blocked_')){
              const reason={blocked_storage:'storage unavailable or full',blocked_auth:'credentials rejected',blocked_conflict:'source history conflict'}[value];
              this.notify('error',`Collector ${stage} blocked: ${reason}. Heartbeats are arriving, but this does not mean synchronization is healthy. Open Fleet for details.`,machine);
            }else if(prior?.startsWith('blocked_'))this.notify('done',`Collector ${stage} reports ready again. This does not mean its backlog has finished synchronizing.`,machine);
          }
          if(before?.state===machine.state)continue;
          if(machine.state==='connected') {
            if(before)this.notify('done','Collector heartbeat restored.',machine);
          } else this.notify('error',machine.state==='delayed'?'Collector heartbeat delayed (over 90 seconds).':'Collector disconnected (over 180 seconds without a heartbeat).',machine);
        }
        this.seen=next;
        this.machineRows=rows;
      } catch {
        if(!this.closed&&!this.hidden()&&!this.failed){this.failed=true;this.notify('error','Cannot check collector status: the hub is unavailable or returned invalid status. This does not prove a collector stopped.',{id:'',name:'AMC hub'});}
      } finally {
        this.pending=false;
        if(!this.closed&&!this.hidden()){this.afterPoll(this.failed?[]:this.machineRows||[]);this.timer=this.schedule(()=>this.poll(),30000);}
      }
    }
    visibilityChanged(){this.cancel(this.timer);if(!this.hidden())this.poll();}
    close(){this.closed=true;this.cancel(this.timer);this.seen.clear();}
  }

  class HookIdeasModel {
    constructor(client,render){this.client=client;this.render=render;this.ticket=0;this.loading=false;this.error='';this.data=null;}
    emit(){if(!this.closed)this.render(this);}
    pause(){this.ticket++;this.controller?.abort();this.secret?.cancel();this.loading=false;}
    close(){this.pause();this.secret?.close();this.closed=true;}
    async load(){
      if(this.closed)return false;
      this.pause();const ticket=this.ticket,controller=this.controller=new AbortController();
      this.loading=true;this.error='';this.emit();
      try{
        await this.batch;if(this.closed||ticket!==this.ticket)return false;
        const pending=Promise.allSettled([this.client.javascriptHookSummary({signal:controller.signal}),this.client.gitUndoSummary({signal:controller.signal}),this.client.modelHookSummary({signal:controller.signal})]);this.batch=pending;
        const results=await pending;if(this.closed||ticket!==this.ticket)return false;
        const failed=results.find(result=>result.status==='rejected');if(failed)throw failed.reason;
        this.data=results[0].value;this.undo=results[1].value;this.models=results[2].value;return true;
      }catch(error){if(!this.closed&&ticket===this.ticket)this.error=error.message;return false;}
      finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.emit();}}
    }
  }
  function checkpointMarkup(value){
    if(!value)return '<p>Database checkpoint status: not reported by this hub.</p>';
    const reclaim=value.reclaimPolicy==='on-wal-reset'?'<p>SQLite reclaims excess WAL space at safe writer resets. The retained-size limit is 64 MiB; active transactions and readers can require a larger WAL. Routine checkpoints do not force truncation.</p>':value.lastReclaim==='reclaimed'?'<p>WAL space reclaimed at the last attempt.</p>':value.lastReclaim==='busy'?'<p>WAL space reclamation deferred at the last attempt: the database was busy. Retrying without evicting readers or waiting for active writers.</p>':value.lastReclaim==='error'?`<p role="alert">WAL space reclamation failed at the last attempt.${value.reclaimProblem?' '+escape(value.reclaimProblem):''}</p>`:value.lastReclaim?`<p>Last WAL space reclamation: ${escape(value.lastReclaim)}.</p>`:'';
    const restart=value.reclaimPolicy==='size-triggered-restart'?'<p>Above 64 MiB, a completed checkpoint attempts WAL recycling with up to 5 ms of lock waiting. Disk I/O can take longer. Existing reader snapshots are preserved; the next safe writer reset reclaims space without forced truncation. Active work can require a larger WAL.</p>'+(value.lastReclaim==='restart-ready'?'<p>Last recycling attempt prepared a safe writer reset.</p>':value.lastReclaim==='busy'?'<p>Last recycling attempt was busy; it will be retried.</p>':value.lastReclaim==='error'?`<p role="alert">WAL recycling failed.${value.reclaimProblem?' '+escape(value.reclaimProblem):''}</p>`:''):reclaim;
    return `<p ${value.state==='error'?'role="alert"':''}>Database checkpoint: ${escape(value.state||'unknown')} · ${escape(value.copiedFrames??'unknown')}/${escape(value.logFrames??'unknown')} WAL frames copied at the last attempt.${value.problem?' '+escape(value.problem):''}</p>`+(value.state==='reader-or-checkpointer-busy'?'<p>Checkpoint contention; retrying without evicting readers. This is separate from upload and indexing backlog.</p>':'')+restart;
  }
  function storageObservationMarkup(value){
    if(!value)return '';
    return `<p ${value.state&&value.state!=='ready'?'role="status"':''}>Storage observation: ${escape(value.checkedAt||'not yet available')}${value.availableBytes!=null?` · last observed free: ${escape(value.availableBytes)} bytes`:''}${value.reserveBytes!=null?` · required reserve: ${escape(value.reserveBytes)} bytes`:''}.${value.probeInProgress?' A free-space check is in progress.':''}${value.problem?' '+escape(value.problem):''}</p>`;
  }
  function ledgerObservationMarkup(value,error){
    if(!value)return error?`<p role="status">Ledger observation unavailable: ${escape(error)}</p>`:'';
    return `<p ${value.state!=='ready'?'role="status"':''}>Ledger observation: ${escape(value.state||'unknown')} · checked ${escape(value.checkedAt||'not yet available')}. Counts and checkpoint details are from that check.${value.probeInProgress?' A refresh is in progress.':''}${value.problem?' '+escape(value.problem):''}</p>`;
  }
  function fleetUndoMarkup(page,summary){
    return '<h2>Graveyard · fleet undo history</h2><p>Recorded direct Git command attempts, not proof that commands succeeded or files were lost. Nothing here executes Git. Wrapper scripts and aliases are not evaluated.</p>'+`<p>Whole-fleet coverage: ${summary.sessions} current sessions, including archived · ${summary.ready} with complete indexed coverage · ${summary.needsRebuild} need rebuilding · ${summary.incomplete} incomplete. ${summary.attempts} known attempts; ${summary.unsupported} unsupported inputs and ${summary.diagnostics} indexing diagnostics. Missing history is not a zero result.</p><form id="v2UndoProjectForm"><label>Recorded project folder (exact; blank means unrecorded)<input id="v2UndoProject" maxlength="4096" value="${escape(page.project??'')}"></label><button type="submit">Filter undo history</button><button type="button" id="v2UndoAllProjects">All project folders</button></form><p>Showing ${page.project==null?'all recorded project folders':page.project===''?'sessions without a recorded project folder':escape(page.project)}. Folder is the session context, not a verified command working directory.</p><p>Newest recorded times first; unknown times last. Rebuilding an old chat does not make it recent.</p>`+(page.events.map((e,i)=>`<article class="ev"><h3>${escape(e.rule)} attempt · ${escape(e.title||e.sessionId)}</h3><p>${escape(e.machineName||e.machineId)} · ${escape(e.provider)} · ${e.archived?'Archived · ':''}${escape(e.timestamp||'Unknown recorded time')}</p><p>Recorded project: ${escape(e.project||'not recorded')} <button data-undo-project="${i}">Show this project</button></p><pre>${escape(e.command)}</pre><p>${e.pathCount} named paths${e.pathCount>e.paths.length?`; showing ${e.paths.length}`:''}</p><ul>${e.paths.map(path=>`<li>${escape(path)}</li>`).join('')}</ul>${e.pathCount===0?`<button data-fleet-prior-edits="${i}">Show earlier edit attempts</button>`:''}<button data-fleet-undo-record="${i}">Open original record ${e.sequence}</button></article>`).join('')||'<p>No recognized attempts on this page of available indexed history.</p>');
  }
  function undoHistoryMarkup(page){
    const rules={reset:'Hard reset attempt',revert:'Revert attempt',checkout:'Path checkout attempt',restore:'Path restore attempt'};
    const head='<p>Recorded direct Git command attempts, not proof that commands succeeded or files were lost. AMC does not execute these commands. Wrapper scripts and aliases are not evaluated.</p>';
    if(page.state==='needs-rebuild')return head+'<p role="status">Undo history needs rebuilding. No historical absence is inferred from this empty page.</p>';
    return head+`<p>${page.attempts} recognized attempts in ${page.indexedOffset} indexed bytes. ${page.unsupported} unsupported inputs; ${page.diagnostics} indexing diagnostics.${page.state==='incomplete-indexing'?' Coverage is incomplete.':''}</p>`+(page.events.map(event=>`<article class="ev"><h3>${escape(rules[event.rule])}</h3><pre>${escape(event.command)}</pre><p>${event.pathCount} named paths${event.pathCount>event.paths.length?`; showing ${event.paths.length}`:''}</p><ul>${event.paths.map(path=>`<li>${escape(path)}</li>`).join('')}</ul>${event.pathCount===0?`<button data-prior-edits="${event.sequence}">Show earlier edit attempts</button>`:''}<button data-undo-record="${event.sequence}">Open original record ${event.sequence}</button></article>`).join('')||'<p>No recognized direct undo attempts on this page of the indexed prefix.</p>');
  }
  function javascriptHookJSON(){
    const command="node -e \"let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{let f='';try{f=(JSON.parse(s).tool_input||{}).file_path||''}catch(e){}if(!/\\.(js|mjs|cjs)$/i.test(f))process.exit(0);try{require('child_process').execFileSync(process.execPath,['--check',f],{stdio:'pipe'})}catch(e){console.error('That file does not parse as JavaScript right now: '+String(e.stderr||e.message));process.exit(2)}})\"";
    return JSON.stringify({hooks:{PostToolUse:[{matcher:'Edit|Write|MultiEdit',hooks:[{type:'command',command}]}]}},null,2);
  }
  function undoHookJSON(){
    const command="node -e \"let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{let c='';try{c=(JSON.parse(s).tool_input||{}).command||''}catch(e){}if(c.indexOf('amc-ok')>=0)process.exit(0);const hit=c.split(/[;\\n]|&&|\\|\\|/).map(x=>x.trim()).find(x=>/^git\\s/.test(x)&&(/\\breset\\b.*--hard\\b/.test(x)||(/\\brevert\\b/.test(x)&&!/--(abort|continue|quit|skip)\\b/.test(x))||/\\bcheckout\\b.*\\s--(\\s|$)/.test(x)||(/\\brestore\\b/.test(x)&&!/--staged/.test(x))));if(!hit)process.exit(0);console.error('Stopped: that throws work away - '+hit+'. Check the Graveyard tab for what was thrown away here before. If you still mean it, add  # amc-ok  to the command.');process.exit(2)})\"";
    return JSON.stringify({hooks:{PreToolUse:[{matcher:'Bash',hooks:[{type:'command',command}]}]}},null,2);
  }
  function undoHookMarkup(summary){
    const head='<section class="hp-card"><h3>Review destructive Git commands before they run</h3>';
    if(!summary)return head+'<p>Undo evidence unavailable. No proposal is shown.</p></section>';
    const proposed=Number.isSafeInteger(summary.readyAttempts)&&summary.readyAttempts>=3;
    return head+`<p>${escape(summary.readyAttempts)} recognized attempts in completely indexed sessions; ${escape(summary.attempts)} known attempts overall. ${escape(summary.ready)} of ${escape(summary.sessions)} sessions have complete coverage, including archived. ${escape(summary.needsRebuild)} need rebuilding; ${escape(summary.incomplete)} are incomplete. Attempts do not prove a command succeeded or work was lost.</p>`+(proposed?`<p>The recorded sample meets the three-attempt threshold. This optional Claude Code Bash hook checks hard reset, revert, path checkout and restore before execution. It can block legitimate work and misread quoted examples; it can miss wrappers and other spellings. It is not a security boundary and does not guard Codex tools.</p><pre class="hp-json">${escape(undoHookJSON())}</pre><button id="v2UndoHookCopy">Copy Git review hook JSON</button><p>Requires Node on the target computer. Review and merge into local Claude settings yourself; never replace existing hooks blindly. The legacy escape marker amc-ok anywhere in the command bypasses this check. Nothing is installed or executed here.</p>`:'<p>Not proposed: at least three recognized attempts from completely indexed sessions are required. Missing or unsupported history is not evidence that no undo occurred.</p>')+'<button id="v2UndoHookEvidence">Review attempts in Graveyard</button></section>';
  }
  function modelHookJSON(){
    const command="node -e \"let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{let i={};try{i=JSON.parse(s).tool_input||{}}catch(e){}if((typeof i.model==='string'&&i.model.trim())||(typeof i.prompt==='string'&&i.prompt.indexOf('amc-ok')>=0))process.exit(0);console.error('Stopped: name a nonempty model or put amc-ok in the prompt to allow an implicit choice.');process.exit(2)})\"";
    return JSON.stringify({hooks:{PreToolUse:[{matcher:'Task',hooks:[{type:'command',command}]}]}},null,2);
  }
  function modelHookMarkup(value){
    const head='<section class="hp-card"><h3>Require an explicit model on future subagent calls</h3>';
    if(!value)return head+'<p>Model attribution unavailable. No proposal is shown.</p></section>';
    const proposed=Number.isSafeInteger(value.withoutRecordedModel)&&Number.isSafeInteger(value.agents)&&value.withoutRecordedModel>=10&&value.withoutRecordedModel<=value.agents&&value.withoutRecordedModel/value.agents>=0.02;
    return head+`<p>${escape(value.withoutRecordedModel)} of ${escape(value.agents)} recorded session/agent pairs have no model attributed by usage observations. ${escape(value.withRecordedModel)} have at least one recorded model; this does not imply complete attribution. ${escape(value.unattributedContexts)} session contexts have records without agent identity and are excluded from that denominator. Archived sessions are included. Counts are not unique fleet processes or spawn attempts. Missing or unindexed history is not measured.</p><p>This does not prove a spawn omitted its model, inherited a model, or incurred avoidable cost. Recorded models do not prove explicit selection.</p>`+(proposed?`<p>The recorded sample meets the optional-policy threshold of 10 agents and 2%. This Claude Task hook requires a nonempty top-level model string; it does not validate model names. It does not guard Codex or differently named tools and is not a security boundary.</p><pre class="hp-json">${escape(modelHookJSON())}</pre><button id="v2ModelHookCopy">Copy explicit-model hook JSON</button><p>Requires Node. Review and merge into existing local Claude settings manually. amc-ok anywhere in the prompt bypasses the check. Nothing is installed or executed here.</p>`:'<p>Not proposed: fewer than 10 agents or 2% lack recorded model attribution. This is not proof that every spawn named a model.</p>')+'</section>';
  }
  function hookIdeasMarkup(state){
    const head='<div class="fleet-head"><h2>Hook ideas</h2></div><p>Copy-only guidance. AMC does not install or execute hooks. Evidence spans the fleet; a hook guards only the computer where you manually configure it.</p>'+(!state.error&&!state.loading&&state.data?undoHookMarkup(state.undo):'');
    const controls=`<button id="v2HooksRefresh" ${state.loading?'disabled':''}>Refresh hook evidence</button><p role="status">${escape(state.error||(state.loading?'Checking fleet evidence…':''))}</p>`;
    if(state.error||state.loading||!state.data)return head+controls+'<p>Evidence is unavailable while this check is pending or failed. No proposal is shown.</p>';
    const d=state.data,e=d.evidence;
    return head+controls+`<p>${d.sessions} current sessions, including archived · ${e.sessionsRead} with usable indexed evidence · ${d.needsRebuild} need evidence rebuilt · ${d.incomplete} incomplete. Uncaptured or undiscovered history is not measured.</p><section class="hp-card"><h3>Check JavaScript syntax after an edit</h3><p>${e.sessionsWithErrors} of ${e.sessionsEdited} JavaScript-editing sessions reported a later JavaScript syntax error; ${e.errors} result records. This sequence does not prove the edit caused the error.</p>${d.proposed?`<p>Supported by the recorded sample. Runs after the edit, using Node's syntax checker; it cannot undo or prevent that edit. Node must be installed on the target computer.</p><pre class="hp-json">${escape(javascriptHookJSON())}</pre><button id="v2HooksCopy">Copy JavaScript hook JSON</button><button id="v2HooksBrain">Open Brain to review settings</button><p>Review the local Claude settings file yourself. Merge this entry into its hooks section without replacing other settings. Nothing here saves the file.</p>`:`<p>Not proposed: this check requires at least five JavaScript-editing sessions with usable evidence and at least one reported error. Missing evidence is not a zero-error result.</p>`}<div>${e.examples.map(x=>`<button data-hook-session="${escape(x.sessionId)}">${escape(x.sessionId)} · ${x.errors} result records</button>`).join('')}</div></section>${modelHookMarkup(state.models)}`;
  }

  function alignTraceSteps(a,b){
    if(!Array.isArray(a)||!Array.isArray(b)||a.length>300||b.length>300)throw new Error('Alignment requires at most 300 steps per side.');
    const equal=(x,y)=>x?.state==='known'&&y?.state==='known'&&x.tool===y.tool&&x.signature===y.signature;
    const width=b.length+1,score=new Int16Array((a.length+1)*width);
    for(let i=1;i<=a.length;i++)score[i*width]=-i;
    for(let j=1;j<=b.length;j++)score[j]=-j;
    for(let i=1;i<=a.length;i++)for(let j=1;j<=b.length;j++)score[i*width+j]=Math.max(score[(i-1)*width+j-1]+(equal(a[i-1],b[j-1])?2:-1),score[(i-1)*width+j]-1,score[i*width+j-1]-1);
    const rows=[];let i=a.length,j=b.length;
    while(i||j){
      let left=null,right=null;
      if(i&&j&&score[i*width+j]===score[(i-1)*width+j-1]+(equal(a[i-1],b[j-1])?2:-1)){left=a[--i];right=b[--j];}
      else if(i&&score[i*width+j]===score[(i-1)*width+j]-1)left=a[--i];else right=b[--j];
      const state=left&&left.state!=='known'||right&&right.state!=='known'?'unknown':equal(left,right)?'same':'different';
      rows.push({left,right,state});
    }
    return rows.reverse();
  }
  function divergenceOutcomeMarkup(source,target){
    const describe=(label,value)=>`<section><h4>${label}</h4>${value?`<p>Latest matched error-result signal: ${value.retrying===true?'present':value.retrying===false?'not indicated':'unknown'}. Current stall heuristic: ${value.stalled===true?'present':value.stalled===false?'not indicated':'unknown'}.</p><p>${value.uncertainCalls} uncertain call identities. ${value.pending?`Recorded call ${value.pending.sequence} has no matched result.`:'No unambiguous unmatched call identified.'}</p><p>Observed ${escape(value.observedAt)}. These signals describe indexed tool evidence, not a whole-task verdict.</p>`:'<p>Lifecycle evidence unavailable.</p>'}</section>`;
    return '<section><h3>Outcome evidence</h3><p>Task success is not established for either run. A returned tool, an empty error count, or an old quiet transcript is not proof of success or a live stall.</p><div class="dl-cols">'+describe('Source run',source)+describe('Selected run',target)+'</div></section>';
  }
  function divergenceTraceMarkup(value){
    if(!value)return '';
    if(value.state!=='ready')return `<p role="status">Comparison unavailable: source child ${escape(value.sourceState)}, selected child ${escape(value.targetState)}.</p>`;
    const cell=step=>step?`<b>${escape(step.tool||'Unknown tool')}</b> · record ${step.sequence}<pre>${escape(step.preview)}</pre>${step.state!=='known'?`<span>Evidence: ${escape(step.state)}</span>`:''}`:'<span>No aligned step</span>';
    return divergenceOutcomeMarkup(value.sourceOutcome,value.targetOutcome)+`<section><h3>Aligned child tool calls</h3><p>Source child ${escape(value.source.childSessionId)} versus selected child ${escape(value.target.childSessionId)}. Exact tool and full JSON-argument signatures; previews below may be abbreviated. This does not establish task success or explain a cause.</p><p>Comparison window ${value.window||1}. Each window is aligned independently; a boundary can change alignment and is not a global trace alignment.</p><p>${value.left.steps.length} source steps · ${value.right.steps.length} selected steps. ${value.left.nextSequence||value.right.nextSequence?'Partial window: additional steps remain; this is not a complete trace comparison.':'Reached the end of both currently indexed traces; missing source history may still exist.'} At most three bounded pages (300 steps) per side are aligned per comparison.</p><p>Source completeness: ${escape(value.left.completeness)} · selected completeness: ${escape(value.right.completeness)}. Unknown evidence is never treated as an equal step. Alignment is one deterministic sequence interpretation, not proof of the first causal divergence.</p><div class="table-wrap"><table class="ftable dl-table"><thead><tr><th>Evidence</th><th>Source run</th><th>Selected run</th></tr></thead><tbody>${value.rows.map(row=>`<tr class="${row.state==='same'?'':'dl-diff'}"><td>${escape(row.state)}</td><td>${cell(row.left)}</td><td>${cell(row.right)}</td></tr>`).join('')}</tbody></table></div>${value.left.nextSequence||value.right.nextSequence?'<button id="v2DivContinueTrace">Next trace window</button>':''}</section>`;
  }
  class DivergenceModel {
    constructor(client,render){this.client=client;this.render=render;this.ticket=0;this.loading=false;this.error='';this.id='';this.anchor=0;this.parentSnapshot='';this.comparison=null;this.pageData=null;this.after=0;this.previous=[];this.child=null;this.selected=null;}
    emit(){if(!this.closed)this.render(this);}
    pause(){this.ticket++;this.controller?.abort();this.loading=false;}
    close(){this.pause();this.closed=true;}
    async run(work){
      if(this.closed)return false;
      this.pause();const ticket=this.ticket,controller=this.controller=new AbortController();this.loading=true;this.error='';this.emit();
      try{await this.batch;if(this.closed||ticket!==this.ticket)return false;
        const pending=work(controller.signal);this.batch=pending.catch(()=>{});const apply=await pending;
        if(this.closed||ticket!==this.ticket)return false;apply();return true;
      }catch(error){if(!this.closed&&ticket===this.ticket)this.error=error.message;return false;}
      finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.emit();}}
    }
    open(id,anchor,parentSnapshot=''){
      if(this.closed)return false;
      this.comparison=null;this.id=id;this.anchor=anchor;this.parentSnapshot=parentSnapshot;this.after=0;this.previous=[];this.pageData=null;this.child=null;this.selected=null;
      return this.load(0,[],'');
    }
    load(after=0,previous=[],snapshot=''){
      this.comparison=null;const id=this.id,anchor=this.anchor;
      return this.run(async signal=>{const page=await this.client.delegationMatches(id,anchor,{after,limit:20,snapshot,signal});
        return ()=>{this.pageData=page;this.after=after;this.previous=previous;this.child=null;this.selected=null;};});
    }
    page(direction){
      if(this.loading||!this.pageData)return false;
      if(direction==='next'){if(!this.pageData.nextSequence)return false;return this.load(this.pageData.nextSequence,[...this.previous,this.after].slice(-20),this.pageData.snapshot);}
      if(!this.previous.length)return false;return this.load(this.previous.at(-1),this.previous.slice(0,-1),this.pageData.snapshot);
    }
    resolve(index){
      if(this.loading)return false;
      const ref=index===-1?{sessionId:this.id,sequence:this.anchor,sessionSnapshot:this.parentSnapshot}:this.pageData?.matches[index];
      if(!ref)return false;this.comparison=null;this.child=null;this.selected=ref;
      return this.run(async signal=>{const child=await this.client.delegationChild(ref.sessionId,ref.sequence,{snapshot:ref.sessionSnapshot,signal});return ()=>{this.child=child;};});
    }
    continueComparison(){
      const value=this.comparison;
      if(this.loading||value?.state!=='ready'||!value.left.nextSequence&&!value.right.nextSequence)return false;
      return this.compare(value.index,value);
    }
    compare(index,prior=null){
      if(this.loading||!this.pageData)return false;
      const ref=this.pageData.matches[index];if(!ref)return false;
      this.comparison=null;const id=this.id,anchor=this.anchor,parentSnapshot=this.parentSnapshot,matchSnapshot=this.pageData.snapshot,after=this.after;
      return this.run(async signal=>{
        const source=await this.client.delegationChild(id,anchor,{snapshot:parentSnapshot,signal});
        if(signal.aborted)throw Error('Comparison cancelled');
        const target=await this.client.delegationChild(ref.sessionId,ref.sequence,{snapshot:ref.sessionSnapshot,signal});
        if(source.state!=='resolved'||target.state!=='resolved')return ()=>{this.comparison={state:'unresolved',sourceState:source.state,targetState:target.state};};
        if(prior&&(source.childSessionId!==prior.source.childSessionId||source.childSnapshot!==prior.source.childSnapshot||target.childSessionId!==prior.target.childSessionId||target.childSnapshot!==prior.target.childSnapshot))throw Error('Child interpretation changed; restart comparison.');
        const read=async(child,previous)=>{
          const result={steps:[],nextSequence:previous?.nextSequence||0,after:previous?.nextSequence||0,completeness:previous?.completeness||''};
          if(previous&&!previous.nextSequence)return result;
          for(let count=0;count<3;count++){
            if(signal.aborted)throw Error('Comparison cancelled');
            const page=await this.client.traceSteps(child.childSessionId,child.childAgentId,{after:result.nextSequence,limit:100,snapshot:child.childSnapshot,signal});
            result.steps.push(...page.steps);result.nextSequence=page.nextSequence;result.completeness=page.completeness;
            if(!page.nextSequence)break;
          }
          return result;
        };
        const left=await read(source,prior?.left),right=await read(target,prior?.right);
        if(signal.aborted)throw Error('Comparison cancelled');
        const sourceOutcome=await this.client.agentLifecycle(source.childSessionId,source.childAgentId,{snapshot:source.childSnapshot,signal});
        if(signal.aborted)throw Error('Comparison cancelled');
        const targetOutcome=await this.client.agentLifecycle(target.childSessionId,target.childAgentId,{snapshot:target.childSnapshot,signal});
        // Revalidate instruction evidence after I/O, not just the child snapshots.
        await this.client.delegationMatches(id,anchor,{after,limit:20,snapshot:matchSnapshot,signal});
        if(signal.aborted)throw Error('Comparison cancelled');
        const rows=alignTraceSteps(left.steps,right.steps);
        return ()=>{this.comparison={state:'ready',index,window:(prior?.window||0)+1,source,target,left,right,rows,sourceOutcome,targetOutcome};};
      });
    }
  }
  function divergenceMarkup(state){
    const head='<div class="fleet-head"><h2>Divergence Ladder</h2></div><p>Find calls given the same complete normalized instruction across indexed history, including archived sessions. Matching instructions do not prove equal environments or successful outcomes.</p>';
    if(!state.id)return head+'<p>Use Deja Vu to find a delegation request, then choose “Find same-instruction runs”. Choose a matching call to compare its recorded child trace.</p>';
    const page=state.pageData;
    return head+`<p>Source: ${escape(state.id)} · record ${state.anchor}</p><p role="status">${escape(state.error||(state.loading?'Loading evidence…':page?`Instruction evidence: ${page.state}`:''))}</p><button id="v2DivRefresh" ${state.loading?'disabled':''}>Refresh matching calls</button><button data-div-child="-1" ${state.loading?'disabled':''}>Inspect source child</button>`+
      (page?'<p>Coverage: indexed fingerprints only. An empty page does not prove that no comparable history exists. Calls are in record order, not ranked by success.</p>'+page.matches.map((ref,i)=>`<article class="ev"><h3>${escape(ref.title||ref.sessionId)}</h3><p>${escape(ref.machineId)} · ${escape(ref.provider)} · ${escape(ref.timestamp||'Timestamp unavailable')}${ref.archived?' · Archived':''}</p><button data-div-call="${i}" ${state.loading?'disabled':''}>Open recorded call</button><button data-div-child="${i}" ${state.loading?'disabled':''}>Inspect child evidence</button><button data-div-compare="${i}" ${state.loading?'disabled':''}>Compare child traces</button></article>`).join('')+`<button id="v2DivBack" ${state.loading||!state.previous.length?'disabled':''}>Previous matching calls</button><button id="v2DivNext" ${state.loading||!page.nextSequence?'disabled':''}>Next matching calls</button><p>Previous retains 20 page positions; no history is removed.</p>`:'')+
      (state.child?`<section><h3>Child relationship evidence</h3><p>${escape(state.child.state)} · parent ${escape(state.selected.sessionId)} · record ${state.selected.sequence}</p>${state.child.state==='resolved'?'<button id="v2DivOpenChild">Open verified child history</button>':''}<p>A recorded relationship does not prove task success. Compare child traces to inspect lifecycle evidence for both runs; successful-run selection remains unfinished.</p></section>`:'')+divergenceTraceMarkup(state.comparison);
  }
  class DejaModel {
    constructor(client,render){this.client=client;this.render=render;this.related=true;this.text='';this.rows=[];this.after=0;this.next=0;this.snapshot='';this.previous=[];this.ticket=0;this.loading=false;this.error='';}
    emit(){if(!this.closed)this.render(this);}
    pause(){this.ticket++;this.controller?.abort();this.loading=false;}
    close(){this.pause();this.closed=true;}
    async search(text=this.text,reset=true,related=this.related){
      if(this.closed)return false;
      if(related!==this.related){reset=true;this.related=related;}
      this.controller?.abort();const controller=this.controller=new AbortController(),ticket=++this.ticket;
      this.text=text.trim();if(reset){this.after=0;this.next=0;this.snapshot='';this.previous=[];this.rows=[];}
      this.loading=!!this.text;this.error='';this.emit();if(!this.text)return true;
      try{
        await this.batch;if(this.closed||ticket!==this.ticket)return false;
        this.batch=this.client.search(this.text,{delegatedOnly:true,related:this.related,after:this.after,snapshot:this.snapshot,limit:this.related?5:20,signal:controller.signal});
        // Keep a settled wait handle so a failed prior read does not block retry.
        const pending=this.batch;this.batch=pending.catch(()=>{});
        const page=await pending;if(this.closed||ticket!==this.ticket)return false;
        this.rows=page.events;this.next=this.related?0:page.nextSequence||0;this.snapshot=page.snapshot;return true;
      }catch(error){if(!this.closed&&ticket===this.ticket)this.error=error.message;return false;}
      finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.emit();}}
    }
    page(direction){if(this.loading)return;if(direction==='next'){if(!this.next)return;this.previous=[...this.previous,this.after].slice(-20);this.after=this.next;}else{if(!this.previous.length)return;this.after=this.previous.pop();}return this.search(this.text,false);}
  }
  function dejaResultLabel(e){
    const result=e.toolResult;
    if(!result)return 'Request recorded; outcome unverified';
    if(result.state==='matched')return result.error===true?'Tool reported failure':result.error===false?'Tool returned without an error flag; task success unverified':'Tool result recorded; error status unknown';
    return ({'missing-result':'No matching tool result recorded','ambiguous':'Multiple matching identities; result ambiguous','missing-id':'Call identity missing; result unverified','missing-agent':'Agent identity missing; result unverified','invalid-time-or-order':'Result timing or order unverified'})[result.state]||'Result evidence unverified';
  }
  function dejaMarkup(state){
    return `<div class="fleet-head"><h2>Deja Vu Finder</h2></div><p>Search recorded delegation requests across complete indexed history, including archived sessions. ${state.related?'Up to five related lexical matches, ranked by the index. Any entered word may match; this is not semantic understanding.':'All entered words must match; results are in record order, with pagination.'} Call/result matching reports recorded tool evidence, not whether the delegated goal was achieved.</p><form id="v2DejaForm"><label>Match mode<select name="mode" ${state.loading?'disabled':''}><option value="related" ${state.related?'selected':''}>Related tasks — top five</option><option value="exact" ${!state.related?'selected':''}>All words — paginated</option></select></label><label>Task to find<input name="text" maxlength="4096" value="${escape(state.text)}" ${state.loading?'disabled':''}></label><button ${state.loading?'disabled':''}>Search delegated tasks</button></form><p role="status">${escape(state.error||(state.loading?'Searching delegated history…':state.text?`${state.rows.length} ${state.related?'ranked matches (maximum five)':'matches on this page'}`:'Enter words from a previous task.'))}</p><div class="deja-results">${state.rows.map((e,i)=>`<button class="deja-row" data-deja-open="${i}"><span class="deja-main"><span class="deja-task">${escape(e.text)}</span><span class="dim">${escape(e.agentDisplayName||e.agentId||'Unattributed identity')} · ${escape(e.timestamp&&!e.timestamp.startsWith('0001-')?e.timestamp:'Timestamp unavailable')} · ${escape(e.searchContext?.sessionTitle||e.sessionId||'Session unavailable')} · ${escape(e.searchContext?.machineName||e.searchContext?.machineId||'Machine unavailable')}${e.searchContext?.archived?' · Archived':''} · ${escape(dejaResultLabel(e))}</span></span></button>`).join('')}</div>${state.text&&!state.loading&&!state.error&&!state.rows.length?'<p>No matching supported delegation requests in indexed history. This does not prove the task is new.</p>':''}<button id="v2DejaFirst" ${state.loading?'disabled':''}>${state.related?'Refresh related matches':'Refresh from first page'}</button><button id="v2DejaBack" ${state.loading||!state.previous.length?'disabled':''}>Previous results</button><button id="v2DejaNext" ${state.loading||!state.next?'disabled':''}>Next results</button>`;
  }
  function galaxyNodes(rows,glyphs=new Map()) {
    if(rows.length>20)throw new Error('Galaxy requires a bounded session page.');
    const machines=[...new Set(rows.map(r=>r.machineId||''))].sort();
    const suns=machines.map((id,i)=>({id,sun:true,x:500+Math.cos(i*2*Math.PI/machines.length)*230,y:300+Math.sin(i*2*Math.PI/machines.length)*170,r:16}));
    const maximum=Math.max(1,...rows.map(r=>Number.isFinite(r.costEstimate)?Math.max(0,r.costEstimate):0));
    const colors={flagship:'#a78bfa',premium:'#f59e0b',mid:'#60a5fa',cheap:'#34d399'};
    const stars=rows.map((row,i)=>{
      const parent=suns.find(s=>s.id===(row.machineId||'')),value=glyphs.get(row.id)?.value;
      const tiers=[...(value?.tiers||[])].sort((a,b)=>b.cost-a.cost||a.tier.localeCompare(b.tier));
      const tier=tiers[0]?.cost>0?tiers[0].tier:'unknown';
      const cost=Number.isFinite(row.costEstimate)&&row.costEstimate>=0?row.costEstimate:null;
      return {id:row.id,parent,sun:false,x:parent.x+Math.cos(i*2.39996)*90,y:parent.y+Math.sin(i*2.39996)*90,r:3+Math.sqrt((cost||0)/maximum)*14,color:colors[tier]||'#94a3b8',title:row.metadata?.name||row.title||row.id,cost,tier,coverage:value?`${(value.tiers||[]).reduce((n,t)=>n+t.pricedTokens,0)}/${value.recordedTokens} recorded tokens tier-classified`:(glyphs.get(row.id)?.error||'Tier evidence loading')};
    });
    const nodes=[...suns,...stars];
    // Bounded settling work; no perpetual animation or corpus-sized node cache.
    for(let step=0;step<120;step++){
      for(const n of nodes){n.dx=0;n.dy=0;}
      for(let i=0;i<nodes.length;i++)for(let j=i+1;j<nodes.length;j++){
        const a=nodes[i],b=nodes[j],dx=a.x-b.x||0.01,dy=a.y-b.y||0.01,d=Math.max(1,Math.hypot(dx,dy));
        const f=Math.min(3,500/(d*d));a.dx+=dx/d*f;a.dy+=dy/d*f;b.dx-=dx/d*f;b.dy-=dy/d*f;
      }
      for(const n of nodes){if(!n.sun){n.dx+=(n.parent.x-n.x)*0.012;n.dy+=(n.parent.y-n.y)*0.012;}n.x=Math.max(35,Math.min(965,n.x+n.dx));n.y=Math.max(45,Math.min(555,n.y+n.dy));}
    }
    return {suns,stars};
  }
  function galaxyFilters(state){
    const q=state.query||{};
    return `<form id="v2GalaxyFilters"><fieldset ${state.loading?'disabled':''}><legend>Galaxy filters</legend><label>Search sessions<input name="q" value="${escape(q.q||'')}"></label><label>Provider<select name="provider">${['','claude','codex','otel'].map(p=>`<option value="${p}" ${q.provider===p?'selected':''}>${p||'All providers'}</option>`).join('')}</select></label><label>Machine ID (exact)<input name="machineId" value="${escape(q.machineId||'')}"></label><label>Project (exact)<input name="project" value="${escape(q.project||'')}"></label><label>Archive visibility<select name="archived">${[['false','Active'],['true','Archived'],['all','All']].map(([v,label])=>`<option value="${v}" ${(q.archived===undefined?'all':String(q.archived))===v?'selected':''}>${label}</option>`).join('')}</select></label><button>Apply filters</button><button type="button" id="v2GalaxyClear">Clear filters</button></fieldset></form>`;
  }
  function galaxyMarkup(state){
    const {suns,stars}=galaxyNodes(state.rows,state.glyphs);
    const pageKey=JSON.stringify(state.rows.map(r=>[r.id,r.machineId]));
    if(state.galaxyView?.pageKey!==pageKey)state.galaxyView={pageKey,zoom:1,x:0,y:0,positions:new Map()};
    for(const star of stars){const saved=state.galaxyView.positions.get(star.id);if(saved){star.x=saved.x;star.y=saved.y;}}
    return `<div class="fleet-head"><h2>Galaxy · ${stars.length} sessions on this page / ${escape(state.total??'unknown')} matching</h2><button id="v2GalaxyRefresh">Refresh</button></div><p role="status">${escape(state.error||(state.loading?'Loading session and tier evidence…':''))}</p><p>Stars link to their source machine, not to inferred agent ancestry. Size shows known estimated cost; colour shows the largest classified historical tier estimate. Grey is unknown, not free. Drag stars or background; scroll to zoom; click a star to open its session.</p><button id="v2GalaxyReset">Reset view</button><button id="v2GalaxyBack" ${state.loading||!state.previous.length?'disabled':''}>Previous page</button><button id="v2GalaxyNext" ${state.loading||!state.nextCursor?'disabled':''}>Next page</button><svg id="v2Galaxy" role="group" aria-label="Session galaxy, current page" viewBox="0 0 1000 600" style="width:100%;background:#080b11;touch-action:none"><g data-galaxy-world>${stars.map((s,i)=>`<line data-galaxy-edge="${i}" x1="${s.parent.x}" y1="${s.parent.y}" x2="${s.x}" y2="${s.y}" stroke="${s.color}" opacity="0.25"/>`).join('')}${suns.map(s=>`<g><circle cx="${s.x}" cy="${s.y}" r="16" fill="#e5e9f0"/><text x="${s.x}" y="${s.y-24}" text-anchor="middle" fill="#cfd6e4">${escape(s.id||'Unknown machine')}</text></g>`).join('')}${stars.map((s,i)=>`<circle data-galaxy-star="${i}" data-session="${escape(s.id)}" role="button" tabindex="0" aria-label="${escape(s.title)}" cx="${s.x}" cy="${s.y}" r="${s.r}" fill="${s.color}"><title>${escape(s.title)} · ${s.cost===null?'Unpriced':'~$'+s.cost.toFixed(6)} · ${escape(s.tier)} · ${escape(s.coverage)}</title></circle>`).join('')}</g></svg>${stars.length?'':'<p>No sessions match this page.</p>'}`;
  }
  function wireGalaxy(svg,open,view={zoom:1,x:0,y:0,positions:new Map()}){
    const world=svg.querySelector('[data-galaxy-world]');let {zoom,x,y}=view,drag=null;
    const point=e=>{const p=svg.createSVGPoint();p.x=e.clientX;p.y=e.clientY;return p.matrixTransform(svg.getScreenCTM().inverse());};
    const draw=()=>{Object.assign(view,{zoom,x,y});world.setAttribute('transform',`translate(${x} ${y}) scale(${zoom})`);};
    draw();
    svg.onwheel=e=>{e.preventDefault();const p=point(e),next=Math.max(0.3,Math.min(4,zoom*(e.deltaY<0?1.1:0.9)));x=p.x-(p.x-x)*next/zoom;y=p.y-(p.y-y)*next/zoom;zoom=next;draw();};
    svg.onpointerdown=e=>{if(e.button!==0)return;const p=point(e);drag={id:e.pointerId,p,startX:e.clientX,startY:e.clientY,moved:false,star:e.target.closest('[data-galaxy-star]')};svg.setPointerCapture(e.pointerId);};
    svg.onpointermove=e=>{if(!drag||drag.id!==e.pointerId)return;const p=point(e);drag.moved ||= Math.hypot(e.clientX-drag.startX,e.clientY-drag.startY)>4;
      if(drag.star){const nx=Number(drag.star.getAttribute('cx'))+(p.x-drag.p.x)/zoom,ny=Number(drag.star.getAttribute('cy'))+(p.y-drag.p.y)/zoom;view.positions.set(drag.star.dataset.session,{x:nx,y:ny});drag.star.setAttribute('cx',nx);drag.star.setAttribute('cy',ny);const edge=svg.querySelector(`[data-galaxy-edge="${drag.star.dataset.galaxyStar}"]`);edge.setAttribute('x2',nx);edge.setAttribute('y2',ny);}else{x+=p.x-drag.p.x;y+=p.y-drag.p.y;draw();}drag.p=p;};
    svg.onpointerup=e=>{if(!drag||drag.id!==e.pointerId)return;const selected=!drag.moved&&drag.star?.dataset.session;drag=null;svg.releasePointerCapture(e.pointerId);if(selected)open(selected);};
    svg.onpointercancel=svg.onlostpointercapture=()=>{drag=null;};
    svg.onkeydown=e=>{const star=e.target.closest('[data-session]');if(star&&(e.key==='Enter'||e.key===' ')){e.preventDefault();open(star.dataset.session);}};
    return ()=>{zoom=1;x=y=0;draw();};
  }

  class FingerprintsModel {
    constructor(client,render){this.client=client;this.render=render;this.query={limit:20,archived:false};this.rows=[];this.glyphs=new Map();this.previous=[];this.cursor='';this.nextCursor='';this.ticket=0;this.loading=false;this.error='';this.size='medium';}
    emit(){if(!this.closed)this.render(this);}
    filter(values){this.query={...this.query,...values,limit:20};return this.load(true);}
    async load(reset=false){
      if(this.closed)return false;if(reset){this.cursor='';this.previous=[];}
      this.controller?.abort();const controller=this.controller=new AbortController();
      const ticket=++this.ticket;this.loading=true;this.error='';this.emit();
      try{
        // Wait for cancelled work to settle before starting a replacement batch.
        await this.batch;if(this.closed||ticket!==this.ticket)return false;
        this.batch=Promise.allSettled([this.client.sessions({...this.query,cursor:this.cursor,signal:controller.signal}),this.client.totals({...this.query,signal:controller.signal})]);
        const reads=await this.batch;const failed=reads.find(r=>r.status==='rejected');if(failed)throw failed.reason;
        const [page,totals]=reads.map(r=>r.value);
        if(this.closed||ticket!==this.ticket)return false;
        this.rows=page.sessions;this.total=totals.sessions;this.nextCursor=page.nextCursor||'';this.glyphs=new Map();this.emit();
        for(let i=0;i<this.rows.length;i+=2){
          if(this.closed||ticket!==this.ticket)return false;
          this.batch=Promise.all(this.rows.slice(i,i+2).map(async row=>{
            let result;try{result={value:await this.client.fingerprint(row.id,{signal:controller.signal})};}catch(e){result={error:e.message};}
            if(!this.closed&&ticket===this.ticket)this.glyphs.set(row.id,result);
          }));
          await this.batch;
          if(this.closed||ticket!==this.ticket)return false;this.emit();
        }
        return true;
      }catch(e){if(!this.closed&&ticket===this.ticket)this.error=e.message;return false;}
      finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.emit();}}
    }
    page(direction){if(this.loading)return;if(direction==='next'){if(!this.nextCursor)return;this.previous=[...this.previous,this.cursor].slice(-20);this.cursor=this.nextCursor;}else{if(!this.previous.length)return;this.cursor=this.previous.pop();}return this.load();}
    pause(){this.ticket++;this.loading=false;this.controller?.abort();}
    close(){this.pause();this.closed=true;}
  }

  // Reuse the cancellation-safe two-request page loader for project summaries.
  class RingsModel extends FingerprintsModel {
    constructor(client,render,timezone=Intl.DateTimeFormat().resolvedOptions().timeZone){
      super({sessions:async options=>{const page=await client.rankedProjectCatalog(options);if(page.items.some(p=>typeof p.id!=='string'))throw new Error('Invalid project catalog.');return{sessions:page.items,nextCursor:page.nextCursor};},totals:async()=>({}),fingerprint:(id,options)=>client.rings({timezone,project:id||undefined,unassignedProject:!id,signal:options.signal})},render);
      this.timezone=timezone;this.metric='trouble';
    }
  }
  function ringsMarkup(state){
    return `<div class="fleet-head"><h2>Projects — rings · ${state.rows.length} on this page</h2><button id="v2RingsRefresh">Refresh</button></div><label>Colour by<select id="v2RingsMetric"><option value="trouble" ${state.metric==='trouble'?'selected':''}>Trouble rate</option><option value="topTier" ${state.metric==='topTier'?'selected':''}>Top-tier spend</option></select></label><p role="status">${escape(state.error||(state.loading?'Loading project weeks…':''))}</p><p>Up to ten weeks per project in ${escape(state.timezone)}, anchored to its latest active session. Older weeks are inward; thicker means more sessions. Grey means fewer than three sessions or incomplete evidence. Trouble counts recorded failed results, not inferred retries or stalls.</p><div class="rings-grid">`+state.rows.map(row=>{
      const item=state.glyphs.get(row.id),data=item?.value,label=row.name||row.id||'Unassigned';
      if(!data)return `<section class="ring-card">${escape(label)} · ${escape(item?.error||'Loading…')}</section>`;
      const max=Math.max(1,...data.weeks.map(w=>w.sessions)),radius=21+Math.max(0,data.weeks.length-1)*9;
      const rings=data.weeks.map((w,i)=>{const m=rhythmMetric(w,state.metric);return `<circle cx="0" cy="0" r="${15+i*9}" fill="none" stroke="${m.color}" opacity="${m.opacity}" stroke-width="${w.sessions?2+Math.round(w.sessions/max*6):2}"><title>${escape(w.week+': '+w.sessions+' sessions · '+m.note+(m.judged?' · '+m.rate+'%':' · insufficient evidence'))}</title></circle>`;}).join('');
      return `<button class="ring-card" data-ring-project="${escape(row.id)}"><svg class="ring-svg" role="img" aria-label="${escape(label)} weekly activity" viewBox="${-radius-4} ${-radius-4} ${(radius+4)*2} ${(radius+4)*2}" width="${(radius+4)*2}" height="${(radius+4)*2}"><circle r="10" fill="var(--panel2)" stroke="var(--line)"/>${rings}</svg><span class="ring-name">${escape(label)}</span><span class="ring-summary">${data.weeks.reduce((n,w)=>n+w.sessions,0)} sessions shown · ${data.olderSessions} older not shown · ${data.undatedSessions} undated</span></button>`;
    }).join('')+`</div><button id="v2RingsBack" ${state.loading||!state.previous.length?'disabled':''}>Previous projects</button><button id="v2RingsNext" ${state.loading||!state.nextCursor?'disabled':''}>Next projects</button>`;
  }

  function fingerprintsMarkup(state){
    const dims={small:[54,22],medium:[92,36],large:[148,54]}[state.size]||[92,36], [w,h]=dims;
    const q=state.query,filters=`<form id="v2FingerprintFilters"><fieldset ${state.loading?'disabled':''}><legend>Session filters</legend><label>Search<input name="q" value="${escape(q.q||'')}"></label><label>Provider<select name="provider">${['','claude','codex','otel'].map(v=>`<option value="${v}" ${v===(q.provider||'')?'selected':''}>${v||'All providers'}</option>`).join('')}</select></label><label>Machine ID<input name="machineId" value="${escape(q.machineId||'')}"></label><label>Project (exact)<input name="project" value="${escape(q.project||'')}"></label><label>Archive visibility<select name="archived">${[['false','Active'],['true','Archived'],['all','All']].map(([v,label])=>`<option value="${v}" ${v===(q.archived==null?'all':String(q.archived))?'selected':''}>${label}</option>`).join('')}</select></label><button>Apply filters</button></fieldset></form>`;
    return filters+`<div class="fleet-head"><h2>Fingerprints · ${escape(state.total??'…')} matching sessions</h2><button id="v2FingerprintRefresh">Refresh</button></div><p role="status">${escape(state.error|| (state.loading?'Loading activity summaries…':''))}</p><label>Tile size<select id="v2FingerprintSize">${['small','medium','large'].map(v=>`<option ${v===state.size?'selected':''}>${v}</option>`).join('')}</select></label><p>24 stretches of complete indexed history. Bars count tool calls, results and spawns; red means a recorded failed result. Border: largest classified historical estimate — purple flagship, amber premium, blue mid, green cheap. Grey means no positive classified estimate. Hover for token coverage; partial coverage is not a full-session classification.</p><div class="fp-wall sz-${escape(state.size)}">`+state.rows.map(row=>{
      const g=state.glyphs.get(row.id),v=g?.value,title=row.metadata?.name||row.title||row.id;
      if(!v)return `<button class="fp-tile" data-fingerprint-open="${escape(row.id)}">${escape(title)} · ${escape(g?.error||'Loading…')}</button>`;
      const max=Math.max(...v.buckets,1),bars=v.buckets.map((n,i)=>n?`<rect x="${i*w/24}" y="${h-3-n/max*(h-6)}" width="${Math.max(.6,w/24-.6)}" height="${n/max*(h-6)}" fill="${v.errors[i]?'var(--red)':'var(--accent)'}"/>`:'').join('');
      const tiers=v.tiers||[],rank=['flagship','premium','mid','cheap'],dominant=[...tiers].sort((a,b)=>b.cost-a.cost||rank.indexOf(a.tier)-rank.indexOf(b.tier))[0];
      const classified=tiers.reduce((n,t)=>n+t.pricedTokens,0),border=dominant&&dominant.cost>0?{flagship:'#a78bfa',premium:'#f59e0b',mid:'#60a5fa',cheap:'#34d399'}[dominant.tier]:'var(--line)';
      const tierLabel=dominant&&dominant.cost>0?`${dominant.tier} largest classified historical estimate · ${classified}/${v.recordedTokens} recorded tokens tier-classified`:`tier unknown · ${classified}/${v.recordedTokens??'unknown'} recorded tokens tier-classified`;
      const details=`${title} · ${pricingDisplay(row).cost} · ${v.events} events · ${v.durationMs==null?'duration unknown':Math.round(v.durationMs/1000)+'s observed span'} · ${v.undatedEvents} undated · ${v.unknownResultStatus} results with unknown status · ${tierLabel}`;
      const tick=v.durationMs==null?'':`<line aria-label="Observed duration" x1="${w-3-Math.min(1,Math.log10(v.durationMs/60000+1)/Math.log10(181))*w*.4}" y1="${h-.75}" x2="${w}" y2="${h-.75}" stroke="var(--accent)" stroke-width="1.5"/>`;
      return `<button class="fp-tile" data-fingerprint-open="${escape(row.id)}" aria-label="${escape(details)}"><svg class="fp-svg" viewBox="0 0 ${w} ${h}" width="${w}" height="${h}"><title>${escape(details)}</title><rect x=".75" y=".75" width="${w-1.5}" height="${h-1.5}" rx="3" fill="var(--panel2)" stroke="${border}"/>${bars}${tick}</svg></button>`;
    }).join('')+`</div><button id="v2FingerprintBack" ${state.loading||!state.previous.length?'disabled':''}>Previous sessions</button><button id="v2FingerprintNext" ${state.loading||!state.nextCursor?'disabled':''}>Next sessions</button>`;
  }

  function groupPriceCells(row) {
    const counts = [row.tokensIn,row.tokensCache,row.tokensCacheWrite,row.tokensOut,row.pricedTokens,row.unpricedTokens];
    const recorded = counts.slice(0,4).reduce((a,b)=>a+b,0);
    const valid = counts.every(n=>Number.isSafeInteger(n)&&n>=0) && Number.isSafeInteger(recorded) && row.pricedTokens+row.unpricedTokens===recorded;
    const cost = valid && row.pricedTokens>0 && typeof row.costEstimate==='number' && Number.isFinite(row.costEstimate) && row.costEstimate>=0 ? '~$'+row.costEstimate.toFixed(6) : 'unavailable';
    return `<td>${cost}</td><td>${valid?escape(row.pricedTokens)+' / '+escape(recorded):'unknown'}</td><td>${valid?escape(row.unpricedTokens):'unknown'}</td>`;
  }

  function economicsCostMarkup(data){
    const money=value=>value==null?'unavailable':'~$'+Number(value).toFixed(6);
    return `<h3>Published cost estimates</h3><p>${escape(data.sessions)} sessions · known estimate ${money(data.knownCost)}. Estimates are not invoices.</p><p>${escape(data.pricedTokens)} / ${escape(data.recordedTokens)} recorded tokens priced; ${escape(data.unpricedOrUnmeasuredTokens)} unpriced or awaiting measurement.</p><p>Cost breakdown covers ${escape(data.components.pricedTokens)} recorded tokens. ${escape(data.pricedTokensWithoutBreakdown)} priced tokens lack historical component evidence. ${escape(data.sessionsWithoutBreakdown)} sessions have no recorded breakdown.</p><table><thead><tr><th>Cost category</th><th>Estimate for covered tokens only</th></tr></thead><tbody>`+
      [['Fresh input','input'],['Cache reads','cacheRead'],['Cache writes','cacheWrite'],['Output','output']].map(([label,key])=>`<tr><td>${label}</td><td>${data.components.pricedTokens?money(data.components[key]):'unavailable'}</td></tr>`).join('')+
      '</tbody></table><p>Unpriced usage is not free. Missing breakdowns are not distributed proportionally. Only published estimates enter this summary; archived history is included unless filtered out.</p><p>Tier comparisons and full historical charts remain under implementation. Standing-order measurement previews are available from the standing-order registry. These panels are not full legacy Economics parity.</p>';
  }

  function economicsLifetimeMarkup(data){
    return `<h3>Observed Claude child-agent lifetimes</h3><p>${escape(data.childSources)} evidenced child sources; ${escape(data.withoutMessageObservations)} without stable usage-bearing message observations; ${escape(data.withUnstableMessageIdentity)} with missing message identity.</p><p>Counts are deduplicated usage-bearing messages, not every chat turn or elapsed lifetime. Missing observations remain unknown. Cost/message uses only fully priced, fully attributed sources in that bucket; its denominator excludes the other sources.</p><table><thead><tr><th>Observed messages</th><th>Child sources</th><th>Messages</th><th>Comparable sources</th><th>Comparable messages</th><th>Estimate/message</th></tr></thead><tbody>`+data.buckets.map(b=>`<tr><td>${escape(b.label)}</td><td>${escape(b.agents)}</td><td>${escape(b.messages)}</td><td>${escape(b.costEligibleAgents)}</td><td>${escape(b.costEligibleMessages)}</td><td>${b.comparableCost===null?'unavailable':'~$'+(b.comparableCost/b.costEligibleMessages).toFixed(6)}</td></tr>`).join('')+'</tbody></table><p>Different histories and pricing dates can affect comparisons. This is descriptive evidence, not a causal model-tier recommendation.</p>';
  }

  const legacyEconomicsIntro='<h3>Preserved legacy economics</h3><p>Original legacy snapshots, not verified Go accounting. Current filters do not apply. Duplicates and malformed or partial records are retained. Missing pricing coverage cannot be inferred from an old cost total.</p><p><a href="/api/v2/analytics/economics/legacy/raw" download>Download exact legacy history bytes</a></p>';
  function legacyEconomicsMarkup(page){
    return legacyEconomicsIntro+'<table><thead><tr><th>Byte offset</th><th>Record quality</th><th>Legacy values / original record</th></tr></thead><tbody>'+page.items.map(item=>{
      let values='';
      if(item.validObject){try{const row=JSON.parse(item.raw);values='<p>'+[['at','Recorded timestamp'],['sessions','Sessions'],['subs','Subagents'],['totalUsd','Legacy estimated USD'],['topTierShare','Legacy top-tier share']].map(([key,label])=>`${label}: ${escape(typeof row[key]==='number'&&Number.isFinite(row[key])?row[key]:'not recorded')}`).join(' · ')+'</p>';}catch{}}
      return `<tr><td>${escape(item.offset)}</td><td>${item.validObject?'JSON object':'Malformed or unsupported'}${item.complete?'':' · incomplete final record'}</td><td>${values}<details><summary>Original record (${escape(item.length)} bytes)</summary><pre>${escape(item.raw)}</pre></details></td></tr>`;
    }).join('')+'</tbody></table>'+(page.items.length?'':'<p>No copied legacy economics history is available.</p>');
  }

  function economicsResolutionMarkup(page) {
    return '<h3>Unavailable capture audit</h3><p>These records acknowledge unavailable attempts, not measurements or proof that a request originally completed. Current session filters do not apply.</p>'+(!page.items.length?'<p>No unavailable attempts recorded.</p>':'<div class="table-scroll"><table><thead><tr><th>Recorded at (UTC)</th><th>Capture ID</th><th>Original restore identity</th><th>Resolution restore identity</th></tr></thead><tbody>'+page.items.map(r=>`<tr><td>${escape(r.resolvedAt)}</td><td>${escape(r.id)}</td><td>${escape(r.originalEpoch||'Unknown')}</td><td>${escape(r.resolvedEpoch)}</td></tr>`).join('')+'</tbody></table></div>');
  }
  function economicsHistoryCharts(page) {
    if(!Array.isArray(page?.items)||page.items.length>100)return '<p>Invalid history chart page.</p>';
    if(!page.items.length)return '';
    const points=page.items.map(item=>{
      const m=item?.measurement,c=m?.costs,time=Date.parse(m?.measuredAt);
      if(!Number.isFinite(time)||!c||![c.recordedTokens,c.pricedTokens,c.unpricedOrUnmeasuredTokens].every(n=>Number.isSafeInteger(n)&&n>=0)||c.pricedTokens>c.recordedTokens||c.unpricedOrUnmeasuredTokens!==c.recordedTokens-c.pricedTokens||c.knownCost!==null&&(typeof c.knownCost!=='number'||!Number.isFinite(c.knownCost)||c.knownCost<0))return null;
      return {time,tokens:c.recordedTokens,cost:c.knownCost,coverage:c.recordedTokens?100*c.pricedTokens/c.recordedTokens:null};
    });
    if(points.some(p=>!p))return '<p>Inconsistent history chart evidence; no trend drawn.</p>';
    points.sort((a,b)=>a.time-b.time);
    const first=points[0].time,last=points.at(-1).time;
    const x=p=>first===last?360:90+540*(p.time-first)/(last-first);
    const date=t=>new Date(t).toISOString();
    const number=n=>Number.isInteger(n)?String(n):n.toPrecision(4);
    const plots=[['Known estimated cost','cost','USD',null],['Recorded tokens','tokens','tokens',null],['Recorded-token pricing coverage','coverage','%',100]].map(([title,key,units,fixed])=>{
      const available=points.filter(p=>p[key]!==null),maximum=fixed??Math.max(0,...available.map(p=>p[key])),scale=maximum||1;
      const marks=available.map(p=>`<circle cx="${x(p).toFixed(2)}" cy="${(165-130*(p[key]/scale)).toFixed(2)}" r="4" fill="currentColor"><title>${escape(date(p.time))}: ${escape(number(p[key]))} ${units}</title></circle>`).join('');
      return `<figure><figcaption>${title} (${units})</figcaption><svg role="img" aria-label="${title} at ${available.length} recorded timestamps; exact values in the table below" viewBox="0 0 720 240" width="720" style="max-width:100%;height:auto"><title>${title}</title><path d="M90 30V165H630" fill="none" stroke="currentColor"/><text x="82" y="169" text-anchor="end" fill="currentColor">0</text><text x="82" y="39" text-anchor="end" fill="currentColor">${escape(number(scale))}</text>${marks}<text x="90" y="190" fill="currentColor">${escape(date(first).slice(0,10))}</text><text x="90" y="209" fill="currentColor">${escape(date(first).slice(11,23))}</text><text x="630" y="190" text-anchor="end" fill="currentColor">${escape(date(last).slice(0,10))}</text><text x="630" y="209" text-anchor="end" fill="currentColor">${escape(date(last).slice(11,23))}</text><text x="360" y="234" text-anchor="middle" fill="currentColor">Measurement time (UTC)</text></svg><p>${points.length-available.length} observations unavailable. ${!available.length?'No known values to plot.':''}</p></figure>`;
    }).join('');
    return '<section aria-label="Historical measurement charts"><p>Dots are saved whole-fleet observations on this page, not daily spending or tokens added between snapshots. Unobserved times are not filled in. Changing index or pricing coverage can change totals; a change does not establish a standing-order effect.</p>'+plots+'</section>';
  }

  function economicsHistoryMarkup(page) {
    return '<h3>Historical fleet measurements</h3><p>Whole fleet, including archived history, as indexed at each sampling time. Current filters do not apply. Missing days are not backfilled; estimates are not invoices, and a change does not establish that a standing order caused it.</p><table><thead><tr><th>Measured at (UTC)</th><th>Reason</th><th>Sessions</th><th>Known estimate</th><th>Priced / recorded tokens</th><th>Unpriced or unmeasured tokens</th></tr></thead><tbody>'+page.items.map(({reason,measurement:m})=>`<tr><td>${escape(m.measuredAt)}</td><td>${escape(reason)}</td><td>${escape(m.costs.sessions)}</td><td>${m.costs.knownCost===null?'unavailable':'~$'+m.costs.knownCost.toFixed(6)}</td><td>${escape(m.costs.pricedTokens)} / ${escape(m.costs.recordedTokens)}</td><td>${escape(m.costs.unpricedOrUnmeasuredTokens)}</td></tr>`).join('')+'</tbody></table>'+(page.items.length?'':'<p>No durable fleet measurements recorded yet.</p>');
  }

  function economicsModelsMarkup(page) {
    if(!Array.isArray(page.groups)||page.groups.length>100)return '<p>Invalid model page.</p>';
    return '<h3>Recorded model economics</h3><p>Model names come from usage evidence, not configured agent roles. Estimates cover directly priced observations only; missing pricing is not free. This is one page, not a whole-fleet tier mix.</p><table><thead><tr><th>Recorded model</th><th>Sessions</th><th>Observations</th><th>Attributable estimate</th><th>Priced / recorded tokens</th><th>Unpriced or unmeasured tokens</th></tr></thead><tbody>'+page.groups.map(g=>`<tr><td>${escape(g.label||g.key||'Unknown model')}</td><td>${escape(g.sessions)}</td><td>${escape(g.observations)}</td>${groupPriceCells(g)}</tr>`).join('')+'</tbody></table>'+(page.groups.length?'':'<p>No recorded models match these filters.</p>');
  }

  // One explicit editor, not an automatically replayed file-write queue. Draft
  // text survives rerenders and errors; a save response can only update its file.
  class BrainDraft {
    constructor(client, changed = () => {}) {
      this.client=client;this.changed=changed;this.file=null;this.text='';this.error='';
      this.loading=false;this.saving=false;this.unknown=false;this.ticket=0;this.closed=false;
    }
    get dirty(){return !!this.file&&this.text!==this.file.content;}
    emit(){if(!this.closed)this.changed(this);}
    edit(text){if(this.closed||this.loading||typeof text!=='string')return false;this.text=text;return true;}
    async open(id,{discard=false}={}) {
      if(this.closed||this.saving)return false;
      if(this.dirty&&!discard){this.error='Keep or explicitly discard the current draft before loading a file.';this.emit();return false;}
      const ticket=++this.ticket;this.loading=true;this.error='';this.emit();
      try {
        const file=await this.client.brainFile(id);
        if(this.closed||ticket!==this.ticket)return false;
        this.file=file;this.text=file.content;this.unknown=false;return true;
      } catch(error){if(!this.closed&&ticket===this.ticket)this.error=error.message;return false;}
      finally {if(!this.closed&&ticket===this.ticket){this.loading=false;this.emit();}}
    }
    async save() {
      if(this.closed||this.loading||this.saving||!this.file||!this.dirty||this.unknown)return false;
      const file=this.file,text=this.text,ticket=this.ticket;
      this.saving=true;this.error='';this.emit();
      try {
        const result=await this.client.saveBrainFile(file.id,text,file.expectedHash);
        if(this.closed||ticket!==this.ticket||this.file!==file)return false;
        if(result.id!==file.id)throw new Error('Save acknowledgement belongs to another file. Reload before saving again.');
        this.file={...file,content:text,expectedHash:result.expectedHash,mtime:result.mtime};
        // Edits made while the save was in flight remain a dirty draft.
        return true;
      } catch(error) {
        if(!this.closed&&ticket===this.ticket){this.error=error.message;this.unknown=error.code==='brain_save_unknown'||!error.code;}
        return false;
      } finally {if(!this.closed&&ticket===this.ticket){this.saving=false;this.emit();}}
    }
    async compareDisk() {
      if(this.closed||this.loading||this.saving||!this.file)return null;
      const file=this.file,ticket=this.ticket;
      try {
        const disk=await this.client.brainFile(file.id);
        if(this.closed||ticket!==this.ticket||this.file!==file)return null;
        // Read-only comparison never rebases the expected hash or discards text.
        return disk;
      }catch(error){if(!this.closed&&ticket===this.ticket){this.error=error.message;this.emit();}return null;}
    }
    close(){this.closed=true;this.ticket++;}
  }

  function pricingDisplay(row) {
    const comparison=row.apiComparison, estimate=comparison?.estimate;
    if(estimate){
      const historical=pricingDisplay({...row,apiComparison:null});
      return {cost:estimate.pricedTokens>0?'API ~$'+Number(estimate.cost).toFixed(3):'API unpriced',
        coverage:`${estimate.pricedTokens}/${estimate.recordedTokens} tokens priced${estimate.unattributedTokens>0?' · partial attribution':''}`,
        detail:`API-rate comparison, not your subscription bill. ${estimate.unpricedTokens} unpriced tokens; ${estimate.unattributedTokens} unattributed tokens. Catalog ${comparison.catalogId}; date ${comparison.comparisonAt}; context ${comparison.context}; estimate ${comparison.id}. Historical estimate: ${historical.cost}. ${historical.detail}`};
    }
    const cost=row.costEstimate==null?'unpriced':'~$'+Number(row.costEstimate).toFixed(3);
    const p=row.pricing;
    if(!p)return {cost,coverage:row.costEstimate==null?'':'coverage unknown',detail:'No checkpoint-bound pricing coverage is available.'};
    return {cost,coverage:p.recordedTokens>0?`${p.pricedTokens}/${p.recordedTokens} tokens priced${p.unattributedTokens>0?' · partial attribution':''}`:'no recorded usage',
      detail:`${p.unpricedTokens} unpriced tokens; ${p.unattributedTokens} unattributed tokens. Catalog ${p.catalogId}; billing context ${p.context}; estimate ${p.snapshotId}.`};
  }

  class MeasurementDraft {
    constructor(client,rule,changed=()=>{}){this.client=client;this.rule={...rule};this.changed=changed;this.preview=null;this.busy=false;this.closed=false;this.finished=false;this.error='';this.result=null;}
    emit(){if(!this.closed)this.changed(this);}
    close(){this.closed=true;this.preview=null;}
    async load(){
      if(this.closed||this.busy||this.finished)return false;
      this.busy=true;this.preview=null;this.error='';this.emit();
      try{const p=await this.client.previewStandingOrderMeasurement(this.rule.id,this.rule.stateHash);if(this.closed)return false;this.preview=Object.freeze({...p});return true;}
      catch(error){if(!this.closed)this.error=error.message;return false;}
      finally{this.busy=false;this.emit();}
    }
    async apply(){
      if(this.closed||this.busy||this.finished||!this.preview)return false;
      this.busy=true;this.error='';this.emit();
      // Never resubmit an uncertain file mutation automatically. The server
      // retains partial application intent; refresh/check before recovery.
      this.finished=true;
      try{const r=await this.client.applyStandingOrderMeasurement(this.preview);if(this.closed)return false;this.result=r;return true;}
      catch(error){if(!this.closed)this.error=error.message+' Refresh the rule and inspect its files before another attempt; some files may already have changed.';return false;}
      finally{this.busy=false;this.emit();}
    }
  }

  function standingOrderMarkup(page,registry){
    return '<h2>Standing orders</h2><p>Local registered guidance rules. A healthy file marker is separate from completion of a pending application.</p><button id="v2OrdersFirst">First page</button><button id="v2OrdersNext" '+(page.nextCursor?'':'disabled')+'>Next page</button><button id="v2OrdersBooks">Playbook library</button>'+
      page.items.map(d=>`<section><h3>${escape(d.title)}</h3><p>${escape(d.topic)} · review every ${escape(d.reviewEveryDays)} days · last reviewed ${escape(d.lastReviewedAt?new Date(d.lastReviewedAt).toISOString():'not recorded')}${d.pendingMeasurement?' · Remeasurement pending':''}</p><pre>${escape(d.body)}</pre><ul>${(d.targets||[]).map(t=>`<li>${escape(t.label||t.name)} · ${escape(t.path)} · application ${escape(t.state||'legacy state unknown')} · applied checksum ${escape(t.appliedHash||'not recorded')}</li>`).join('')}</ul><button data-order-check="${escape(d.id)}">Check files for ${escape(d.title)}</button><div data-order-status="${escape(d.id)}"></div><button data-order-review="${escape(d.id)}" ${d.pendingMeasurement||!d.stateHash?'disabled':''}>Mark ${escape(d.title)} reviewed</button><div role="status" data-review-status="${escape(d.id)}"></div></section>`).join('')+
      '<h3>Registered repository roots</h3><p>Adding a root authorizes local guidance operations within it. Removing an added root does not remove existing guidance files or revoke access granted separately by configuration or local discovery.</p><label>Full repository path <input id="v2RootPath" maxlength="400"></label><button id="v2RootAdd">Add repository root</button><button id="v2RootRefresh">Refresh registry</button><p id="v2RootStatus" role="status"></p><ul>'+registry.roots.map(r=>`<li>${escape(r.label)} · ${escape(r.path)} · ${r.ok?'available':'unavailable'} <button data-root-remove="${escape(r.path)}">Remove added root ${escape(r.label)}</button></li>`).join('')+'</ul><h3>Allowed local file targets</h3><p>No file is changed by viewing this registry.</p><ul>'+registry.targets.map(t=>`<li>${escape(t.label||t.name)} · ${escape(t.path)}</li>`).join('')+'</ul>';
  }

  function contributionTotalsMarkup(totals) {
    return `<h3>Filtered contribution totals</h3><p>Snapshot of the table filters captured when opened; not live-updating. Close and reopen after changing filters.</p><p>${escape(totals.sessions)} session records · ${escape(totals.activeExclusions)} observations excluded using verified evidence · ${escape(totals.staleSelections)} stale decisions not applied.</p><table><thead><tr><th>Tokens</th><th>Recorded</th><th>Verified usage excluded</th><th>Remaining contribution</th></tr></thead><tbody>`+[['tokensIn','Input'],['tokensCache','Cache read'],['tokensCacheWrite','Cache write'],['tokensOut','Output'],['total','Total']].map(([key,label])=>`<tr><th>${label}</th><td>${escape(totals.recorded[key])}</td><td>${escape(totals.excluded[key])}</td><td>${escape(totals.counted[key])}</td></tr>`).join('')+'</tbody></table><p>Partial reconciliation only: unmatched duplicates and inherited counters may remain. Remaining contribution is not certified unique or billable usage. Recorded history and historical prices are unchanged.</p>';
  }

  function contributionMarkup(page) {
    return '<h3>Recorded usage and verified exclusions</h3><p>This page shows current contribution decisions, not fully deduplicated or billable usage. Inherited counters and unmatched copies may remain. Original token observations are preserved; no price or invoice is implied.</p><table><thead><tr><th>Recorded at</th><th>Model</th><th>Input / cache read / cache write / output</th><th>Contribution decision</th></tr></thead><tbody>'+page.items.map(item=>{
      const u=item.recorded;
      const reason=item.exclusionKind==='fork-baseline-v1'?'Verified inherited baseline excluded':'Verified repeat excluded';
      return `<tr><td>${escape(u.timestamp||'Unknown')}</td><td>${escape(u.model||'Unattributed')}</td><td>${[u.tokensIn,u.tokensCache,u.tokensCacheWrite,u.tokensOut].map(escape).join(' / ')}</td><td>${item.counted?'Retained; no current exclusion':`${reason} · <button data-contribution-owner="${escape(item.ownerSessionId)}">Open source session</button><details><summary>Proof reference</summary><code>${escape(item.excludedByProof)}</code></details>`}</td></tr>`;
    }).join('')+'</tbody></table>'+(!page.items.length?'<p>No recorded usage on this page.</p>':'')+`<p>Page contents only—not session or fleet totals.</p><button id="v2ContributionsFirst">Refresh first contribution page</button><button id="v2ContributionsNext" ${page.nextCursor?'':'disabled'}>Next contribution page</button>`;
  }

  function coverageSummary(totals) {
    if(totals?.recordedTokens==null)return 'Pricing coverage unavailable';
    if(!totals.recordedTokens)return 'No recorded token usage';
    const comparison=totals.currentComparisons;
    const current=comparison?` · API-rate comparison (not your bill): ${comparison.costEstimate==null?'unpriced':'~$'+Number(comparison.costEstimate).toFixed(2)}; ${comparison.pricedTokens}/${comparison.recordedTokens} tokens covered across ${comparison.sessionsWithComparison}/${comparison.sessions} sessions; per-session rate dates and assumptions may differ`:'';
    return (comparison?'Historical: ':'')+`${totals.pricedTokens}/${totals.recordedTokens} recorded tokens priced · ${totals.unpricedTokens} unpriced`+
      (totals.tokensAwaitingPricing?` · ${totals.tokensAwaitingPricing} awaiting a current estimate`:'')+
      (totals.knownUnattributedTokens?` · at least ${totals.knownUnattributedTokens} unattributed`:'')+current;
  }

  function policyStatus(policy) {
    if(!policy)return 'Automatic pricing disabled. Existing estimates are retained.';
    if(policy.comparisonAt)return 'API-rate comparison at '+policy.comparisonAt+' (not a bill). '+(policy.problem?'Blocked; retry scheduled: '+policy.problem:policy.id?'Queued or in progress.':'Caught up; saved in estimate history.')+' Historical estimates remain unchanged.';
    if(policy.problem)return 'Pricing blocked; retry scheduled: '+policy.problem;
    return policy.id?'Pricing queued or in progress. The existing estimate is retained until completion.':'Automatic pricing caught up. Unpriced tokens may remain when rates or attribution are missing.';
  }

  function economicsSamplerMarkup(status) {
    if(!status)return '<p>Economics history sampler status unavailable.</p>';
    const stamp=value=>typeof value==='string'&&!value.startsWith('0001-')&&Number.isFinite(Date.parse(value))?escape(value):'not observed in this process';
    return `<section aria-label="Economics history sampler"><p>Economics history sampler: ${escape(status.state||'unknown')}. These observations describe this process, not the latest saved history.</p><p>Last attempt: ${stamp(status.lastAttempt)} · Last capture by this process: ${stamp(status.lastCapture)} · Next attempt: ${typeof status.nextAttempt==='string'&&!status.nextAttempt.startsWith('0001-')&&Number.isFinite(Date.parse(status.nextAttempt))?escape(status.nextAttempt):'not scheduled'}</p>${status.problem?`<p role="alert">${escape(status.problem)}</p>`:''}</section>`;
  }

  function machineVersionMarkup(collectorVersion,hubVersion) {
    const reported=typeof collectorVersion==='string'&&collectorVersion.trim()?collectorVersion:null;
    const hub=typeof hubVersion==='string'&&hubVersion.trim()?hubVersion:null;
    const message=!reported?'Collector version is not reported.':!hub?'Hub version is unavailable; comparison is unknown.':reported!==hub?'Reported version differs from the hub.':'Reported version matches the hub; this does not identify the executable build.';
    return `<p>Collector version: ${escape(reported||'unknown')} · Hub version: ${escape(hub||'unknown')}<br><span ${reported&&hub&&reported!==hub?'class="mver drift"':''}>${escape(message)}</span></p>`;
  }

  function machineNetworkMarkup(report) {
    if(!report)return '<p>IP addresses: not reported by this collector.</p>';
    if(!['reported','unavailable'].includes(report.state)||!Number.isFinite(Date.parse(report.observedAt))||!Array.isArray(report.addresses)||report.addresses.length>32||report.addresses.some(v=>typeof v!=='string'||v.length>128))return '<p>IP addresses: report unavailable or invalid.</p>';
    if(report.state==='unavailable')return `<p>IP address discovery was unavailable at ${escape(report.observedAt)}.</p>`;
    return `<div class="mips">${report.addresses.map(value=>`<span class="ip">${escape(value)}</span>`).join(' ')||'<span class="ip dim">No non-loopback unicast addresses reported</span>'}</div><p>Addresses observed ${escape(report.observedAt)}${report.truncated?' · Report limited to 32 addresses':''}. These are reported interface addresses, not proof of reachability.</p>`;
  }

  function fleetCards(live, history) {
    const cards=new Map(history.map(item=>[item.id,{...item,name:item.name||item.id,connection:'no-heartbeat'}]));
    for(const entry of live){const h=entry.machine.heartbeat,label=entry.machine.label;cards.set(h.machineId,{...cards.get(h.machineId),id:h.machineId,name:label?.machineId===h.machineId&&typeof label.displayName==='string'&&label.displayName?label.displayName:h.name||h.machineId,
      connection:entry.connection,lastSeen:entry.machine.lastSeen,age:entry.connectionAgeSeconds,version:h.version,network:h.network,state:h.state,error:h.error,
      collectionState:h.collectionState||h.state,uploadState:h.connectionState||'unknown',uploadRetryAt:h.uploadRetryAt,
	  historyGaps:Number.isSafeInteger(h.historyGaps)&&h.historyGaps>=0?h.historyGaps:null,
      captured:h.capturedBytes,uploaded:h.uploadedBytes,backlog:h.backlogBytes,lastUpload:h.lastUploadAt});}
    return [...cards.values()].sort((a,b)=>a.name.localeCompare(b.name));
  }

  // A draft is independent of refreshed rows. Send only changed fields, so
  // editing a note cannot overwrite a newer pin, name or project from elsewhere.
  class OrganizationDraft {
    constructor(row) {
      const m = row.metadata || {};
      this.initial = { name: m.name || '', note: m.note || '', tags: (m.tags || []).join('\n'),
        project: m.projectOverride || m.project ? m.project || '' : row.project || '' };
      this.values = { ...this.initial }; this.forceProject = false;
    }
    patch() {
      const patch = {};
      for (const key of ['name','note','tags','project']) {
        if (this.values[key] !== this.initial[key] || (key === 'project' && this.forceProject))
          patch[key] = key === 'tags' ? [...new Set(this.values.tags.split(/\r?\n/).map(s => s.trim()).filter(Boolean))] : this.values[key];
      }
      return patch;
    }
    unassign() { this.values.project = ''; this.forceProject = true; }
  }

  // A fresh page needs current state, not a replay of years of change records.
  // Sample BEFORE loading: commits during the load are caught on the next poll.
  class LiveRefresh {
    constructor({ head, reload, onError = () => {}, hidden = () => false, schedule = (fn, ms) => setTimeout(fn, ms), cancel = id => clearTimeout(id), random = Math.random }) {
      Object.assign(this, { head, reload, onError, hidden, schedule, cancel, random });
      this.observed = null; this.failures = 0; this.closed = false; this.flight = null; this.timer = null;
    }
    poll() {
      if (this.closed) return Promise.resolve();
      if (this.flight) return this.flight;
      this.flight = (async () => {
        try {
          const head = await this.head();
          if (this.closed) return;
          if (!this.observed || this.failures > 0 || head.sequence !== this.observed.sequence || head.recoveryEpoch !== this.observed.recoveryEpoch) {
            // A replaced query or failed load must not consume invalidation.
            if (!await this.reload()) return;
            if (this.closed) return;
            this.observed = head;
          }
          this.failures = 0;
        } catch (error) {
          this.failures = Math.min(this.failures + 1, 8);
          this.onError(error);
          if (error.status === 401 || error.status === 403 || error.code === 'hub_identity_mismatch') this.failures = 8;
        }
      })().finally(() => { this.flight = null; });
      return this.flight;
    }
    async start() {
      if (this.started || this.closed) return;
      this.started = true;
      const run = async () => {
        await this.poll();
        if (this.closed) return;
        const base = this.hidden() ? 10000 : 2000;
        const delay = this.failures ? Math.min(300000, base * 2 ** this.failures) * (0.8 + this.random() * 0.2) : base;
        this.timer = this.schedule(run, delay);
      };
      await run();
    }
    close() { this.closed = true; if (this.timer != null) this.cancel(this.timer); }
  }

  // One page plus a bounded cursor stack, never a whole-fleet cache. A newer
  // query wins even when an older response belongs to a different filter key.
  function projectFilterMarkup(q) {
    return `<input id="v2Project" aria-label="Project (exact)" placeholder="Project (exact)" value="${escape(q.project || '')}" ${q.unassignedProject?'disabled':''}><label><input type="checkbox" id="v2UnassignedProject" ${q.unassignedProject?'checked':''}>Unassigned project only</label>`;
  }
  class TableModel {
    constructor(client, render) {
      this.client = client; this.render = render; this.ticket = 0;
      this.query = { limit: 100, sort: 'lastActivity', direction: 'desc', archived: false, pinnedFirst: true };
      this.rows = []; this.totals = null; this.cursor = null; this.nextCursor = null; this.previous = [];
      this.loading = false; this.totalsLoading = false; this.totalsError = ''; this.error = ''; this.pending = new Map();
    }
    snapshot() {
      const rows = this.rows.map(row => ({ ...row, metadata: this.client.organization(row.id) }));
      return { rows: rows.filter(row => this.query.archived == null || row.metadata.archived === this.query.archived), totals: this.totals,
        loading: this.loading, totalsLoading: this.totalsLoading, totalsError: this.totalsError, error: this.error, hasPrevious: this.previous.length > 0, hasNext: !!this.nextCursor,
        query: { ...this.query }, pending: [...this.pending.values()] };
    }
    emit() { this.render(this.snapshot()); }
    organization(update) {
      const wasPending = this.pending.has(update.sessionID);
      if (update.metadata.pending) this.pending.set(update.sessionID, { id: update.sessionID, state: update.metadata.pendingState });
      else this.pending.delete(update.sessionID);
      // Accepting a 100-row response must not repaint the table 100 times.
      if (update.metadata.pending || wasPending || !this.loading) this.emit();
    }
    async load(reset = false) {
      if (this.closed) return false;
      if (reset) { this.cursor = null; this.previous = []; this.rows = []; this.nextCursor = null; }
      const ticket = ++this.ticket;
      const query = { ...this.query }, cursor = this.cursor;
      this.loading = true; this.totalsLoading = true; if(reset)this.totals = null; this.totalsError = ''; this.error = ''; this.emit();
      const pageRequest = async () => {
        try {
          const page = await this.client.sessions({ ...query, cursor });
          if (ticket !== this.ticket) return false;
          this.rows = page.sessions; this.nextCursor = page.nextCursor;
          return true;
        } catch (error) { if (ticket === this.ticket) this.error = error.message; return false; }
        finally { if (ticket === this.ticket) { this.loading = false; this.emit(); } }
      };
      const totalsRequest = async () => {
        try {
          const totals = await this.client.analyticsTotals(query);
          if (ticket !== this.ticket) return false;
          this.totals = totals;
          return true;
        } catch (error) { if (ticket === this.ticket) this.totalsError = error.message; return false; }
        finally { if (ticket === this.ticket) { this.totalsLoading = false; this.emit(); } }
      };
      const results = await Promise.all([pageRequest(), totalsRequest()]);
      return ticket === this.ticket && results.every(Boolean);
    }
    filter(patch) { Object.assign(this.query, patch); return this.load(true); }
    projectFilter(project, unassigned = false) { return this.filter({project:unassigned?'':project,unassignedProject:unassigned}); }
    next() { if (!this.nextCursor || this.loading) return; this.previous.push(this.cursor); if (this.previous.length > 1000) this.previous.shift(); this.cursor = this.nextCursor; return this.load(); }
    back() { if (!this.previous.length || this.loading) return; this.cursor = this.previous.pop(); return this.load(); }
    archive(id) { const done = this.client.archive(id, !this.client.organization(id).archived); done.then(() => this.load(), error => { this.error = error.message; this.emit(); }); return done; }
    organize(id, patch) {
      const done = this.client.organize(id, patch);
      done.then(() => this.load(Object.hasOwn(patch,'pinned') && this.query.pinnedFirst), error => { if (!this.closed) { this.error = error.message; this.emit(); } });
      return done;
    }
    close() { this.closed = true; this.ticket++; }
  }

  function usageChart(rows, metric='tokens') {
    const names={tokens:'Recorded tokens (including cache)',sessions:'Sessions in group',observations:'Usage observations'};
    if(!Object.hasOwn(names,metric))return '<p>Chart metric unavailable.</p>';
    if(rows.length>100)return '<p>Chart page exceeds the 100-group limit.</p>';
    if(!rows.length)return '<p>No usage groups on this page.</p>';
    const values=rows.map(row=>{
      const parts=metric==='tokens'?[row.tokensIn,row.tokensCache,row.tokensCacheWrite,row.tokensOut]:[row[metric]];
      if(parts.some(v=>!Number.isSafeInteger(v)||v<0))return null;
      const sum=parts.reduce((a,b)=>a+b,0);return Number.isSafeInteger(sum)?sum:null;
    });
    const maximum=Math.max(1,...values.filter(v=>v!==null));
    const bars=rows.map((row,i)=>{
      const label=String(row.label||row.key||'Unknown'),value=values[i],text=value===null?'unknown':String(value),y=42+i*32;
      return `<g><title>${escape(label)}: ${text}</title><text x="8" y="${y+16}" fill="currentColor" font-size="14">${escape(label.length>23?label.slice(0,20)+'…':label)}</text>${value===null?'':`<rect x="205" y="${y}" width="${(value/maximum*380).toFixed(2)}" height="22" fill="var(--accent, #4C72B0)"/>`}<text x="598" y="${y+16}" fill="currentColor" font-size="14">${text}</text></g>`;
    }).join('');
    return `<figure><figcaption>${escape(names[metric])} · current page only · zero-based scale. Exact values are in the table below.</figcaption><div class="usage-chart-wrap" style="max-height:420px;overflow:auto"><svg role="img" aria-label="${escape(names[metric])} for ${rows.length} groups on this page" width="780" height="${rows.length*32+48}" viewBox="0 0 780 ${rows.length*32+48}"><title>${escape(names[metric])}; current group page, not whole-history totals</title><text x="205" y="22" fill="currentColor" font-size="14">0</text><text x="585" y="22" text-anchor="end" fill="currentColor" font-size="14">${maximum}</text>${bars}</svg></div></figure>`;
  }

  class RhythmModel {
    constructor(client,render,timezone=Intl.DateTimeFormat().resolvedOptions().timeZone){Object.assign(this,{client,render,timezone,data:null,metric:'trouble',loading:false,error:'',ticket:0,closed:false});}
    async load(){if(this.closed)return false;const ticket=++this.ticket;this.loading=true;this.error='';this.render(this);try{const data=await this.client.rhythm({timezone:this.timezone,archived:false});if(this.closed||ticket!==this.ticket)return false;this.data=data;return true;}catch(error){if(!this.closed&&ticket===this.ticket)this.error=error.message;return false;}finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.render(this);}}}
    close(){this.closed=true;this.ticket++;}
  }
  function rhythmMetric(b,metric){
    const tier=metric==='topTier',rate=tier?(b.classifiedCost>0?Math.round(100*b.topTierCost/b.classifiedCost):null):(b.sessions?Math.round(100*b.sessionsWithErrors/b.sessions):null);
    const complete=tier?b.recordedTokens>0&&b.tierPricedTokens===b.recordedTokens:b.sessionsWithIncompleteResults===0;
    const judged=b.sessions>=3&&rate!==null&&complete;
    const hi=tier?66:30,lo=tier?33:10;
    return {rate,judged,color:!b.sessions?'var(--line)':!judged?'var(--dim)':rate>hi?'var(--red)':rate>=lo?'var(--amber)':'var(--green)',opacity:!b.sessions ? 0.35 : !judged ? 0.55 : 0.92,
      note:tier?`${b.topTierCost===null?'unknown':'~$'+b.topTierCost.toFixed(6)} top-tier / ${b.classifiedCost===null?'unknown':'~$'+b.classifiedCost.toFixed(6)} classified estimates; ${b.tierPricedTokens}/${b.pricedTokens} priced tokens classified; ${b.pricedTokens}/${b.recordedTokens} recorded tokens priced`:`${b.sessionsWithErrors}/${b.sessions} sessions have recorded failed tool results; ${b.sessionsWithIncompleteResults} have incomplete result evidence`};
  }
  function rhythmOvernight(data){
    const sum=list=>list.reduce((a,b)=>({n:a.n+b.sessions,errors:a.errors+b.sessionsWithErrors,incomplete:a.incomplete+b.sessionsWithIncompleteResults}),{n:0,errors:0,incomplete:0});
    const night=sum(data.hours.slice(0,6)),day=sum(data.hours.slice(6));
    if(night.n<15||day.n<15)return `Not enough sessions for an overnight comparison: ${night.n} started 12am–6am and ${day.n} during the day/evening. At least 15 on each side are required.`;
    const n=100*night.errors/night.n,d=100*day.errors/day.n;
    if(night.incomplete||day.incomplete)return `Overnight: ${n.toFixed(1)}% with recorded errors; day/evening: ${d.toFixed(1)}%. Incomplete result evidence prevents a comparative verdict (${night.incomplete} overnight, ${day.incomplete} daytime sessions).`;
    const se=(p,count)=>Math.sqrt(p/100*(1-p/100)/count),band=200*Math.max(se(n,night.n),se(d,day.n));
    return `Overnight: ${n.toFixed(1)}% with recorded errors (${night.n} sessions); day/evening: ${d.toFixed(1)}% (${day.n} sessions). ${Math.abs(n-d)<=Math.max(10,band)?'Too close to distinguish at this sample size.':n>d?'The recorded overnight error share is higher.':'The recorded overnight error share is lower.'} This is an observational comparison, not evidence that starting time caused the difference.`;
  }
  function rhythmMarkup(state){
    const header=`<div class="fleet-head"><h2>Rhythm — when you run, and how it goes</h2><select id="v2RhythmMetric" aria-label="Rhythm metric"><option value="trouble" ${state.metric==='trouble'?'selected':''}>Trouble rate</option><option value="topTier" ${state.metric==='topTier'?'selected':''}>Top-tier spend</option></select><button id="v2RhythmRefresh">Refresh rhythm</button></div><p role="status">${escape(state.error||(state.loading?'Loading rhythm…':''))}</p>`;
    const data=state.data;if(!data)return header;
    const weekdays=['Mon','Tue','Wed','Thu','Fri','Sat','Sun'];
    const polar=(r,a)=>{const rad=(a-90)*Math.PI/180;return [(130+r*Math.cos(rad)).toFixed(1),(130+r*Math.sin(rad)).toFixed(1)].join(',');};
    const maxHour=Math.max(1,...data.hours.map(b=>b.sessions)),maxDay=Math.max(1,...data.weekdays.map(b=>b.sessions));
    const title=(label,b,m)=>`${label}: ${b.sessions} sessions; ${m.note}${m.rate===null?'':`; ${m.rate}%`}${m.judged?'':'; insufficient sample or incomplete evidence; colour withheld'}`;
    const wedges=data.hours.map(b=>{const m=rhythmMetric(b,state.metric),a=b.index*15-6.5,z=b.index*15+6.5,r=b.sessions?26+92*b.sessions/maxHour:30;return `<path d="M${polar(26,a)} L${polar(r,a)} A${r},${r} 0 0 1 ${polar(r,z)} L${polar(26,z)} A26,26 0 0 0 ${polar(26,a)} Z" fill="${m.color}" opacity="${m.opacity}"><title>${escape(title(String(b.index).padStart(2,'0')+':00',b,m))}</title></path>`;}).join('');
    const ticks=[[0,'12am'],[6,'6am'],[12,'12pm'],[18,'6pm']].map(([h,label])=>{const [x,y]=polar(133,h*15).split(',');return `<text x="${x}" y="${Number(y)+4}" text-anchor="middle" class="rhy-tick">${label}</text>`;}).join('');
    const bars=data.weekdays.map(b=>{const m=rhythmMetric(b,state.metric),h=b.sessions/maxDay*90,x=b.index*44,overlay=m.judged?h*m.rate/100:0;return `<g><title>${escape(title(weekdays[b.index],b,m))}</title><rect x="${x}" y="${90-h}" width="34" height="${h}" fill="var(--accent2)" opacity=".55"/><rect x="${x}" y="${90-overlay}" width="34" height="${overlay}" fill="${m.color}"/><text x="${x+17}" y="106" text-anchor="middle" class="rhy-wd-label">${weekdays[b.index]}</text></g>`;}).join('');
    return header+`<p>All active history · ${escape(data.timezone)} · earliest recorded activity, not an inferred start time. ${data.sessions} dated sessions; ${data.undatedSessions} undated and ${data.outsideRangeSessions} outside the selected range. ${state.error||state.loading?'Showing the last successful snapshot.':''}</p><div class="rings-legend">Spoke length and bar height show session volume. Colour needs at least three sessions and complete evidence for the selected metric. Trouble means recorded failed tool results; retries and stalls are not inferred. Top-tier share uses classified historical estimates, not all spend. Hover for pricing coverage.</div><div class="rhy-wrap"><div class="rhy-col"><svg role="img" aria-label="Sessions by local starting hour" viewBox="-16 -16 292 292" width="292" height="292" class="rhy-clock">${wedges}<circle cx="130" cy="130" r="22" fill="var(--panel2)"/><text x="130" y="127" text-anchor="middle" class="rhy-center-n">${data.sessions}</text><text x="130" y="143" text-anchor="middle" class="rhy-center-l">sessions</text>${ticks}</svg></div><div class="rhy-col"><h3 class="rhy-h3">By day of week</h3><svg role="img" aria-label="Sessions by local weekday" viewBox="0 0 298 116" width="298" height="116">${bars}</svg><p class="rhy-chrono">${escape(rhythmOvernight(data))}</p></div></div>`;
  }

  function calendarRange(now=new Date()) {
    const key=d=>`${d.getFullYear()}-${String(d.getMonth()+1).padStart(2,'0')}-${String(d.getDate()).padStart(2,'0')}`;
    const start=new Date(now);start.setHours(0,0,0,0);start.setDate(start.getDate()-370);start.setDate(start.getDate()-start.getDay());
    const end=new Date(now);end.setHours(0,0,0,0);end.setDate(end.getDate()+1);
    return {start:key(start),end:key(end),timezone:Intl.DateTimeFormat().resolvedOptions().timeZone};
  }
  class CalendarModel {
    constructor(client,render,range=calendarRange()){this.client=client;this.render=render;this.query={...range,archived:false,limit:100};this.days=[];this.metric='sessions';this.loading=false;this.error='';this.ticket=0;}
    async load(){
      if(this.closed)return false;
      const ticket=++this.ticket,query={...this.query};this.loading=true;this.error='';this.render(this);
      try{
        const days=[],seen=new Set();let cursor=null,snapshot='';
        do {
          const page=await this.client.calendar({...query,cursor,snapshot});
          if(this.closed||ticket!==this.ticket)return false;
          if(snapshot&&page.snapshot!==snapshot)throw new Error('Calendar changed during loading; refresh to retry.');
          snapshot=page.snapshot;
          if(days.length+page.days.length>400)throw new Error('Calendar exceeds the bounded year range.');
          for(const day of page.days){if(seen.has(day.day))throw new Error('Calendar repeated a day.');seen.add(day.day);days.push(day);}
          if(page.nextCursor===cursor&&cursor)throw new Error('Calendar cursor did not advance.');
          cursor=page.nextCursor;
        }while(cursor);
        const expected=(Date.parse(query.end)-Date.parse(query.start))/86400000;
        if(days.length!==expected)throw new Error('Calendar day coverage is incomplete.');
        this.days=days;
        if(this.selectedDay){
          const selected=days.find(d=>d.day===this.selectedDay.day);
          if(!selected)this.closeDay();else {this.selectedDay=selected;await this.dayModel.load(true);}
        }
        return !this.closed&&ticket===this.ticket;
      }catch(error){if(!this.closed&&ticket===this.ticket)this.error=error.message;return false;}
      finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.render(this);}}
    }
    selectDay(key){
      if(this.closed)return Promise.resolve(false);
      if(this.selectedDay?.day===key){this.closeDay();return Promise.resolve(true);}
      const day=this.days.find(d=>d.day===key);if(!day)return Promise.resolve(false);
      this.dayModel?.close();this.selectedDay=day;
      this.dayModel=new TableModel(this.client,()=>{if(!this.closed)this.render(this);});
      this.dayModel.query={limit:100,sort:'lastActivity',direction:'desc',archived:false,from:day.from,to:day.to};
      return this.dayModel.load();
    }
    closeDay(){this.dayModel?.close();this.dayModel=null;this.selectedDay=null;if(!this.closed)this.render(this);}
    close(){this.closed=true;this.ticket++;this.dayModel?.close();}
  }
  function calendarDayMarkup(state){
    if(!state.selectedDay||!state.dayModel)return '';
    const page=state.dayModel.snapshot();
    return `<section class="cal-day-panel" aria-label="Selected calendar day"><div class="cal-day-head"><b>${escape(state.selectedDay.day)}</b><span>${escape(page.totals?.sessions??'…')} current matching sessions · ${page.rows.length} on this page</span><button id="v2CalendarDayClose" class="mini-btn">close</button></div>
      <p>Current session records for this day; the calendar grid is the last complete snapshot.</p><p>${escape(coverageSummary(page.totals))}</p><p role="status">${escape(page.error||page.totalsError||(page.loading?'Loading day sessions…':page.totalsLoading?'Updating day totals…':''))}</p>
      <div class="cal-day-list">${page.rows.map(row=>{const price=pricingDisplay(row);return `<button class="cal-day-item" data-calendar-session="${escape(row.id)}"><span class="cdi-t">${escape(row.metadata.name||row.title||row.id)}</span><span class="cdi-m">${escape(row.provider)} · ${escape(row.machineId)} · ${escape(price.cost)} · ${escape(price.coverage)}</span></button>`;}).join('')||(!page.loading&&!page.error?'<p>No active sessions on this page.</p>':'')}</div>
      <button id="v2CalendarDayRefresh">Refresh day</button><button id="v2CalendarDayBack" ${page.loading||!page.hasPrevious?'disabled':''}>Previous sessions</button><button id="v2CalendarDayNext" ${page.loading||!page.hasNext?'disabled':''}>Next sessions</button></section>`;
  }
  function calendarMarkup(state){
    if(!Array.isArray(state.days)||state.days.length>400)throw new Error('Invalid calendar.');
    const metrics={sessions:['Sessions','sessions','#5eead4'],cost:['Cost','costEstimate','#818cf8'],errors:['Errors','errors','#f87171'],agents:['Agents','agentScopes','#60a5fa'],topTier:['Top-tier $','topTierCost','#f87171']};
    const selected=metrics[state.metric]||metrics.sessions;
    const maximum=Math.max(1,...state.days.map(d=>d[selected[1]]??0));
    const best=state.days.reduce((best,d)=>!best||d.sessions>best.sessions?d:best,null);
    const months=['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];
    let previousMonth='';
    const monthLabels=state.days.filter((_,i)=>i%7===0).map(d=>{const month=d.day.slice(0,7),label=month!==previousMonth?months[Number(d.day.slice(5,7))-1]:'';previousMonth=month;return `<span class="cal-month-label">${label||''}</span>`;}).join('');
    return `<div class="fleet-head"><h2>Calendar — last 12 months</h2><label for="v2CalendarMetric">Colour metric</label><select id="v2CalendarMetric">${Object.entries(metrics).map(([key,m])=>`<option value="${key}" ${state.metric===key?'selected':''}>${m[0]}</option>`).join('')}</select><button id="v2CalendarRefresh">Refresh calendar</button></div>
      <p>Latest recorded session activity by local day (${escape(state.query.timezone)}); active sessions only. Undated sessions are excluded. This is not a daily token-spend timeline.</p>
      <p>${best?.sessions?`Busiest day: ${escape(best.day)} — ${escape(best.sessions)} sessions.`:state.loading||state.error?'Calendar totals are not yet available.':'No active sessions in this range.'}</p>
      <p role="status">${escape(state.error||(state.loading?'Loading calendar…':''))}${state.loading&&state.days.length?' Previous results remain visible until the refresh finishes.':''}</p>
      <div class="cal-wrap" style="overflow:auto"><div aria-hidden="true" style="display:grid;grid-auto-flow:column;grid-auto-columns:21px;width:max-content;margin-left:30px;height:20px">${monthLabels}</div><div style="display:flex;gap:4px"><div aria-hidden="true" style="display:grid;grid-template-rows:repeat(7,18px);gap:3px;width:26px;flex-shrink:0">${['','Mon','','Wed','','Fri',''].map(day=>`<span class="cal-wd-label">${day}</span>`).join('')}</div><div style="display:grid;grid-template-rows:repeat(7,18px);grid-auto-flow:column;grid-auto-columns:18px;gap:3px;width:max-content">${state.days.map(d=>{
        const value=d[selected[1]],known=value!=null;
        const tip=`${d.day}: ${d.sessions} sessions; ${selected[0]}: ${known?value:'unavailable'}; ${d.sessionsWithEstimate}/${d.sessions} sessions have estimates; ${d.pricedTokens}/${d.recordedTokens} tokens priced; ${d.tierPricedTokens??0}/${d.pricedTokens} priced tokens have a known tier`;
        return `<button data-calendar-day="${escape(d.day)}" aria-pressed="${state.selectedDay?.day===d.day}" aria-label="${escape(tip)}" title="${escape(tip)}" style="width:18px;height:18px;padding:0;border:1px solid var(--border);outline:${state.selectedDay?.day===d.day?'2px solid currentColor':'none'};background:${known&&value>0?selected[2]:'var(--panel2)'};opacity:${known&&value>0?Math.max(.28,value/maximum):1}">${known?'':'?'}</button>`;
      }).join('')}</div></div></div><p>Sunday to Saturday down each column · less to more colour = ${selected[0].toLowerCase()} · ? = unavailable. Select a day to open its session panel below.</p>${calendarDayMarkup(state)}
      <p>Costs are recorded estimates with incomplete pricing shown in each day's label; unknown is not zero. Errors count recorded failed tool results. Agents count identified scopes within each session, not people or currently running processes. Top-tier cost is the known flagship/premium portion from the selected immutable rate catalogs. Unknown tiers are excluded, with classified-token coverage shown for every day; zero known top-tier cost does not classify the missing portion.</p>`;
  }
  class ProjectPicker {
    constructor(client,render){this.client=client;this.render=render;this.items=[];this.cursor='';this.nextCursor='';this.previous=[];this.loading=false;this.ticket=0;this.error='';}
    async load(){if(this.closed)return;const ticket=++this.ticket;this.loading=true;this.error='';this.render(this);
      try{const page=await this.client.projects({cursor:this.cursor,limit:20});if(this.closed||ticket!==this.ticket)return;this.items=page.items;this.nextCursor=page.nextCursor||'';}
      catch(e){if(!this.closed&&ticket===this.ticket)this.error=e.message;}
      finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.render(this);}}
    }
    page(direction){if(this.loading)return;if(direction==='next'){if(!this.nextCursor)return;this.previous=[...this.previous,this.cursor].slice(-20);this.cursor=this.nextCursor;}else{if(!this.previous.length)return;this.cursor=this.previous.pop();}return this.load();}
    close(){this.closed=true;this.ticket++;}
  }

  class ProjectsModel {
    constructor(client,render){this.client=client;this.render=render;this.columns=[];this.cursor='';this.nextCursor='';this.previous=[];this.ticket=0;this.loading=false;this.saving=false;this.error='';this.pending=[];this.moves=new Map();}
    emit(){if(!this.closed)this.render(this);}
    recover(){try{this.pending=this.client.pendingProjectOperations();}catch(e){this.error=e.message;}}
    refresh(){if(this.closed||this.loading||this.saving)return Promise.resolve(false);return this.load(true);}
    async load(preservePages=false){
      if(this.closed)return false;const ticket=++this.ticket;this.loading=true;this.error='';this.recover();this.emit();
      try{
        const page=await this.client.projects({limit:5,cursor:this.cursor});if(this.closed||ticket!==this.ticket)return false;
        const columns=[];
        for(const project of [...page.items,{id:'',name:'Unassigned',color:'#8a93a8'}]){
          const old=preservePages?this.columns.find(c=>c.project.id===project.id):null;
          const query={projectAssignment:project.id,archived:false,limit:20,...(old?.cursor?{cursor:old.cursor}:{})};
          const [rows,totals]=await Promise.all([this.client.sessions(query),this.client.totals(query)]);
          if(this.closed||ticket!==this.ticket)return false;
          for(const row of rows.sessions){const m=this.client.organization(row.id);if(m.pending&&(m.project||'')!==project.id)this.moves.set(row.id,{row,target:m.project||''});}
          columns.push({project,rows:rows.sessions,total:totals.sessions,nextCursor:rows.nextCursor,cursor:old?.cursor||'',previous:old?[...old.previous]:[]});
        }
        this.columns=columns;this.nextCursor=page.nextCursor||'';return true;
      }catch(e){if(!this.closed&&ticket===this.ticket)this.error=e.message;return false;}
      finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.emit();if(this.refreshAfterLoad){this.refreshAfterLoad=false;this.load();}}}
    }
    async pageColumn(id,direction){
      const c=this.columns.find(c=>c.project.id===id);if(!c||this.loading||this.saving)return;
      const cursor=direction==='next'?c.nextCursor:c.previous.at(-1);if(cursor==null||direction==='next'&&!cursor)return;
      const ticket=++this.ticket;this.loading=true;this.emit();
      try{const page=await this.client.sessions({projectAssignment:id,archived:false,limit:20,cursor});if(this.closed||ticket!==this.ticket)return;
        if(direction==='next')c.previous=[...c.previous,c.cursor].slice(-20);else c.previous.pop();c.cursor=cursor;c.rows=page.sessions;c.nextCursor=page.nextCursor;
      }catch(e){if(!this.closed&&ticket===this.ticket)this.error=e.message;}
      finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.emit();if(this.refreshAfterLoad){this.refreshAfterLoad=false;this.load();}}}
    }
    page(direction){if(this.loading||this.saving)return;if(direction==='next'){if(!this.nextCursor)return;this.previous=[...this.previous,this.cursor].slice(-20);this.cursor=this.nextCursor;}else{if(!this.previous.length)return;this.cursor=this.previous.pop();}return this.load();}
    async save(project,remove=false){
      if(this.closed||this.saving||this.pending.length)return false;
      return this.submit({id:project.id||'project-'+this.client.uuid(),name:project.name||'',color:project.color||'#8a93a8',revision:project.revision||0,delete:remove,operationId:this.client.uuid(),recoveryEpoch:this.client.recoveryEpoch});
    }
    async submit(operation){
      if(this.closed||this.saving)return false;this.saving=true;this.deleting=operation.delete;this.deleteProgress=0;this.error='';this.emit();let success=false;
      try{await this.client.mutateProject(operation);success=true;}
      catch(e){this.error=e.code==='history_changed'?'This saved project operation belongs to restored history. Preserve it for reconciliation; no new write was made.':e.message;}
      finally{this.saving=false;this.deleting=false;this.recover();this.emit();}
      if(success&&!this.closed)await this.load();return success;
    }
    organization(update){
      if(this.closed||!this.moves.has(update.sessionID))return;
      if(!update.metadata.pending){this.moves.delete(update.sessionID);if(this.loading)this.refreshAfterLoad=true;else this.load();}
      else this.emit();
    }
    move(id,target,targetName=''){
      if(this.closed||this.moves.has(id)||typeof target!=='string'||target.length>1024)return;
      const row=this.columns.flatMap(c=>c.rows).find(r=>r.id===id);if(!row)return;
      const metadata=this.client.organization(id);if((metadata.project||'')===target)return;
      let done;try{done=this.client.organize(id,{project:target});}catch(e){this.error=e.message;this.emit();return;}
      this.moves.set(id,{row,target,targetName});this.emit();
      done.then(()=>{const pending=this.moves.delete(id);if(!this.closed&&pending)this.load();},e=>{this.error=e.message;if(!this.client.organization(id).pending)this.moves.delete(id);this.emit();});return done;
    }
    close(){this.closed=true;this.ticket++;}
  }

  function projectsMarkup(state){
    const locked=state.loading||state.saving,cols=state.columns;
    return `<div class="fleet-head"><h2>Projects</h2><button id="v2ProjectNew" ${locked||state.pending.length?'disabled':''}>+ New project</button><button id="v2ProjectRefresh" ${locked?'disabled':''}>Refresh projects</button></div><p role="status">${escape(state.error||(state.saving?(state.deleting?'Deleting project in batches — '+(state.deleteProgress||0)+' memberships cleared; completion pending.':'Saving project…'):state.loading?'Loading projects…':''))}${state.moves.size?' · '+state.moves.size+' session moves pending':''}</p>`+
      (state.moves.size?'<ul>'+[...state.moves.values()].map(m=>`<li>Move pending: ${escape(m.row.metadata?.name||m.row.title||m.row.id)} → ${escape(m.targetName||m.target||'Unassigned')}</li>`).join('')+'</ul>':'')+
      (state.pending.length?`<section><h3>Unresolved project operations</h3><p>Retry checks the same saved operation; it does not create a replacement.</p>${state.pending.map((p,i)=>`<button data-project-retry="${i}" ${state.saving?'disabled':''}>Retry ${escape(p.delete?'delete':p.name)} (${escape(p.id)})</button>`).join('')}</section>`:'')+
      `<div class="proj-board">${cols.map(c=>{
        const p=c.project,color=/^#[a-f0-9]{6}$/i.test(p.color)?p.color:'#8a93a8';
        const rows=[...[...state.moves.values()].filter(m=>m.target===p.id).map(m=>m.row),...c.rows.filter(r=>!state.moves.has(r.id))].slice(0,20);
        return `<section class="proj-col" data-project-drop="${escape(p.id)}" style="--pc:${color}"><div class="proj-col-head"><span class="pdot" style="background:${color}"></span><span class="pcn">${escape(p.name)}</span><span class="pcc">${escape(c.total)}</span>${p.id?`<button data-project-edit="${escape(p.id)}" ${locked||state.pending.length?'disabled':''}>Edit ${escape(p.name)}</button><button data-project-audit="${escape(p.id)}">History</button>`:''}</div><div class="proj-drop">${rows.map(r=>{
          const moving=state.moves.has(r.id),title=r.metadata?.name||r.title||r.id;
          return `<article class="pchip" draggable="${!moving}" data-project-session="${escape(r.id)}"><button data-project-open="${escape(r.id)}">${escape(title)}</button><div class="pchip-m">${escape(r.provider)} · ${escape(r.machineId)} · ${escape(pricingDisplay(r).cost)}${moving?' · move pending':''}</div><label>Move session<select data-project-move="${escape(r.id)}" ${moving?'disabled':''}>${cols.map(d=>`<option value="${escape(d.project.id)}" ${d.project.id===p.id?'selected':''}>${escape(d.project.name)}</option>`).join('')}</select></label><button data-project-picker="${escape(r.id)}" ${moving?'disabled':''}>Choose another project</button></article>`;
        }).join('')||'<p class="proj-empty">Drop sessions here</p>'}</div><button data-project-back="${escape(p.id)}" ${locked||!c.previous.length?'disabled':''}>Previous sessions</button><button data-project-next="${escape(p.id)}" ${locked||!c.nextCursor?'disabled':''}>Next sessions</button></section>`;
      }).join('')}</div><p>Up to five named projects and 20 active sessions per column. Counts are server totals from the last read; pending moves may not be included yet. Unassigned means no owner-assigned project, independent of source repository paths. Archived chats remain in history.</p><button id="v2ProjectsBack" ${locked||!state.previous.length?'disabled':''}>Previous projects</button><button id="v2ProjectsNext" ${locked||!state.nextCursor?'disabled':''}>Next projects</button><section id="v2ProjectEditor"></section><section id="v2ProjectAudit"></section>`;
  }

  class UsageModel {
    constructor(client,render){this.client=client;this.render=render;this.dimension='day';this.query={limit:100};this.rows=[];this.totals=null;this.cursor=null;this.nextCursor=null;this.previous=[];this.ticket=0;this.loading=false;this.error='';}
    emit(){this.render(this);}
    setMetric(metric){if(this.closed||!['tokens','sessions','observations'].includes(metric))return;this.metric=metric;this.emit();}
    async load(reset=false){
      if(this.closed)return false;
      if(reset){this.cursor=null;this.nextCursor=null;this.previous=[];this.rows=[];this.totals=null;}
      const ticket=++this.ticket;this.loading=true;this.error='';this.emit();
      try{
        const [page,totals]=await Promise.all([this.client.usageGroups(this.dimension,{...this.query,cursor:this.cursor}),this.client.analyticsTotals(this.query)]);
        if(this.closed||ticket!==this.ticket)return false;
        this.rows=page.groups;this.nextCursor=page.nextCursor;this.totals=totals;return true;
      }catch(e){if(!this.closed&&ticket===this.ticket)this.error=e.message;return false;}
      finally{if(!this.closed&&ticket===this.ticket){this.loading=false;this.emit();}}
    }
    filter(patch,dimension=this.dimension){this.dimension=dimension;Object.assign(this.query,patch);return this.load(true);}
    next(){if(this.loading||!this.nextCursor)return;this.previous.push(this.cursor);if(this.previous.length>1000)this.previous.shift();this.cursor=this.nextCursor;return this.load();}
    back(){if(this.loading||!this.previous.length)return;this.cursor=this.previous.pop();return this.load();}
    close(){this.closed=true;this.ticket++;}
  }

  async function historyPage(client,id,{after=0,snapshot='',anchor=null,direction='around'}={}) {
    if(anchor==null){
      const page=await client.events(id,{limit:100,after,snapshot});
      return {...page,hasPrevious:after>0};
    }
    const backward=direction==='before';
    const page=await client.eventsAround(id,anchor,{before:backward?100:25,after:backward?0:25,snapshot});
    const events=backward?page.events.filter(e=>e.sequence<anchor):page.events;
    return {...page,events,hasPrevious:page.beforeSequence!=null,
      nextSequence:(backward||page.afterSequence!=null)&&events.length?events.at(-1).sequence:null};
  }

  function boardMarkup(page, stats) {
    if(!Array.isArray(page?.agents)||page.agents.length>100) throw new Error('Invalid board page.');
    const count=value=>Number.isSafeInteger(value)&&value>=0?escape(value):'unknown';
    return `<h3>Recorded agent board</h3><p>${count(stats?.agentCount)} recorded identities across indexed session history; ${page.agents.length} on this page. These are historical identities, not active-agent counts.</p><div class="v2-agent-board" style="display:grid;grid-template-columns:repeat(auto-fill,minmax(min(260px,100%),1fr));gap:16px;margin:16px 0">${page.agents.map(agent=>{
      const values=[agent.tokensIn,agent.tokensCache,agent.tokensCacheWrite,agent.tokensOut];
      const total=values.every(n=>Number.isSafeInteger(n)&&n>=0)?values.reduce((a,b)=>a+b,0):null;
      const known=Number.isSafeInteger(total)?total:null;
      const p=agent.pricing;
      const validPrice=known!=null&&p?.recordedTokens===known&&Number.isSafeInteger(p.pricedTokens)&&p.pricedTokens>=0&&p.pricedTokens<=known&&p.unpricedTokens===known-p.pricedTokens;
      const price=validPrice?pricingDisplay({costEstimate:p.pricedTokens>0&&typeof agent.costEstimate==='number'&&Number.isFinite(agent.costEstimate)&&agent.costEstimate>=0?agent.costEstimate:null,pricing:p}):{cost:'Unpriced',coverage:'Coverage unavailable',detail:'No attributable pricing snapshot'};
      return `<article class="card${agent.kind==='main'?' orchestrator':''}" style="position:relative;width:auto;min-width:0;overflow-wrap:anywhere"><h4>${escape(agent.name||agent.id||'Unattributed agent')}</h4>${agent.name?`<p>Identity: ${escape(agent.id)}</p>`:''}<p>${escape(agent.kind||'Unclassified')} · live status not inferred</p><p>Events: ${count(agent.events)} · tool calls: ${count(agent.toolCalls)} · recorded errors: ${count(agent.errors)}</p><p>Input: ${count(agent.tokensIn)} · cache reads: ${count(agent.tokensCache)} · cache writes: ${count(agent.tokensCacheWrite)} · output: ${count(agent.tokensOut)}</p><p>Unknown-model or incomplete-attribution tokens: ${count(agent.unknownModelTokens)}</p><p title="${escape(price.detail)}">${escape(price.cost)} · ${escape(price.coverage)}</p>${agent.id?`<button data-rename-agent="${escape(agent.id)}" ${agent.namePending?'disabled':''}>Rename agent</button><p role="status">${agent.namePending?'Name change pending; refresh after reconnection.':''}</p>`:''}</article>`;
    }).join('')||'<p>No recorded identities on this page.</p>'}</div>`;
  }

  function agentToolsMarkup(page) {
    if(!Array.isArray(page?.tools)||page.tools.length>100||!Number.isSafeInteger(page.throughSequence)||page.throughSequence<0||page.tools.some(t=>typeof t.name!=='string'||!Number.isSafeInteger(t.calls)||t.calls<1))throw new Error('Invalid historical tool summary.');
    return `<h3>Full-history tool usage</h3><p>Calls across this agent's indexed history through record ${page.throughSequence}, ranked by count. Refresh the first tool page to include newer calls.</p><div class="chips">${page.tools.map(t=>`<span class="chip">${escape(t.name||'Unknown tool')} ×${t.calls}</span>`).join('')||'No recorded tool calls.'}</div>`;
  }

  function agentActivityMarkup(events) {
    if(!Array.isArray(events)||events.length>100)throw new Error('Invalid agent activity page.');
    const tools=new Map();
    for(const e of events) {
      if(!Number.isSafeInteger(e.sequence)||e.sequence<1)throw new Error('Invalid agent event.');
      if(e.kind==='tool-call') {
        const tool=typeof e.data?.tool==='string'?e.data.tool:'Unknown tool';
        tools.set(tool,(tools.get(tool)||0)+1);
      }
    }
    return `<p>Tool calls on this page (not lifetime totals):</p><div class="chips">${[...tools].sort((a,b)=>b[1]-a[1]||a[0].localeCompare(b[0])).map(([tool,count])=>`<span class="chip">${escape(tool)} ×${count}</span>`).join('')||'None on this page.'}</div><p>Indexed text may be abbreviated. Open a record for surrounding session history and complete-source download.</p>${events.map(e=>`<article class="ev"><button data-agent-record="${e.sequence}">Open record ${e.sequence}</button> <strong>${escape(e.kind)}</strong> ${escape(e.timestamp)}<pre style="white-space:pre-wrap;overflow-wrap:anywhere">${escape(e.text||'')}</pre></article>`).join('')||'<p>No indexed records for this identity.</p>'}`;
  }

  function storyMarkup(events) {
    if(!Array.isArray(events)||events.length>100)throw new Error('Invalid story page.');
    const blocks=[];let burst=[];
    const agentLabel=e=>e.agentDisplayName||(e.agentId==='main'?'Orchestrator':e.agentId||'Unattributed');
    const record=event=>`<button data-timeline-sequence="${event.sequence}">Inspect record ${event.sequence}</button>`;
    const flush=()=>{if(!burst.length)return;
      const errors=burst.filter(e=>e.data?.error===true||e.kind==='indexing-error').length;
      const tools=new Map();let replies=0;
      for(const e of burst){
        if(e.kind==='tool-call'){const tool=typeof e.data?.tool==='string'?e.data.tool:'Unknown tool';tools.set(tool,(tools.get(tool)||0)+1);}
        if(e.kind==='assistant-text'&&e.agentId&&e.agentId!=='main')replies++;
      }
      const summary=[...tools].sort((a,b)=>b[1]-a[1]||a[0].localeCompare(b[0])).map(([tool,count])=>`${escape(tool)} ×${count}`).join(' · ');
      blocks.push(`<details class="st-burst ${errors?'has-err':''}"><summary class="st-burst-head">⚙ ${burst.length} recorded activities${errors?` · ${errors} recorded errors`:''}${summary?' · '+summary:''}${replies?` · ${replies} child replies`:''}</summary><div class="st-burst-body">${burst.map(e=>`<div class="st-act"><span class="st-act-who">${escape(agentLabel(e))}</span><span class="st-act-what">${escape(e.data?.tool||e.kind)}</span><div class="st-txt">${escape(e.text||'No indexed text.')}</div>${record(e)}</div>`).join('')}</div></details>`);burst=[];};
    for(const event of events){
      if(!Number.isSafeInteger(event.sequence)||event.sequence<1)throw new Error('Invalid story event.');
      if(event.agentId==='main'&&['user-text','user-queued','assistant-text'].includes(event.kind)){
        flush();const reply=event.kind==='assistant-text';
        blocks.push(`<div class="st-msg ${reply?'st-ai':'st-user'}"><div class="st-who">${reply?escape(agentLabel(event)):'You'} · ${escape(event.timestamp||'Timestamp unavailable')}</div><div class="st-txt">${escape(event.text||'No indexed text.')}</div>${record(event)}</div>`);
      }else burst.push(event);
    }
    flush();
    return `<p>Story for this page of ${events.length} indexed records. Activity groups stop at page boundaries. Indexed text may be abbreviated; inspect a record for source evidence. No task duration or success is inferred.</p><div class="story-inner">${blocks.join('')||'<p>No events on this page.</p>'}</div>`;
  }
  function costFlowMarkup(agents, metric='cost', whole=null, top=null) {
    if(!Array.isArray(agents)||agents.length>100||!['cost','output'].includes(metric))throw new Error('Invalid cost-flow page');
    const money=value=>'~$'+value.toFixed(6), unit=value=>metric==='cost'?money(value):value+' output tokens';
    const rows=agents.map(a=>{
      if(!Number.isSafeInteger(a.tokensOut)||a.tokensOut<0||a.costEstimate!=null&&(!Number.isFinite(a.costEstimate)||a.costEstimate<0))throw new Error('Invalid cost-flow accounting');
      return {name:a.name|| (a.id==='main'?'Orchestrator':a.id||'Unattributed'),weight:metric==='cost'?a.costEstimate:a.tokensOut};
    });
    const known=rows.filter(r=>r.weight!=null),pageTotal=known.reduce((sum,r)=>sum+r.weight,0);
    const wholeValue=whole?(metric==='cost'?whole.costEstimate:whole.tokensOut):undefined;
    if(whole&&(wholeValue!=null&&(!Number.isFinite(wholeValue)||wholeValue<0)||wholeValue==null&&pageTotal>0||wholeValue!=null&&pageTotal>wholeValue+1e-9))throw new Error('Cost-flow page exceeds its whole-session total');
    const total=whole?wholeValue??0:pageTotal;
    if(!Number.isFinite(total))throw new Error('Invalid cost-flow total');
    if(top&&(!Array.isArray(top)||top.length>14||top.some(r=>!Number.isFinite(r.value)||r.value<=0)))throw new Error('Invalid ranked cost-flow contributors');
    const ranked=top?top.map(r=>({name:r.name||(r.id==='main'?'Orchestrator':r.id||'Unattributed'),weight:r.value})):known;
    const chartTotal=ranked.reduce((sum,r)=>sum+r.weight,0);
    if(chartTotal>total+1e-9)throw new Error('Ranked cost flow exceeds its whole-session total');
    const positive=ranked.filter(r=>r.weight>0).sort((a,b)=>b.weight-a.weight),drawn=positive.slice(0,14);
    if(positive.length>14)drawn.push({name:(positive.length-14)+' smaller identities',weight:positive.slice(14).reduce((sum,r)=>sum+r.weight,0)});
    if(whole&&total>chartTotal+1e-9)drawn.push({name:top?'Other identities / unattributed':'Outside this page / unattributed',weight:total-chartTotal});
    const H=Math.max(300,drawn.length*40+40),plot=H-40-drawn.length*6;
    let y=20,svg='';
    for(const row of drawn){
      const h=row.weight/total*plot,pct=(100*row.weight/total).toFixed(1),end=y+h;
      svg+=`<path class="cf-rib" fill="var(--accent2)" opacity=".55" d="M 180 ${y} C 360 ${y},360 ${y},540 ${y} L 540 ${end} C 360 ${end},360 ${end},180 ${end} Z"><title>${escape(row.name)}: ${unit(row.weight)} (${pct}% of ${whole?'known whole-session':'known page'} weight)</title></path><text class="cf-lbl" x="550" y="${y+h/2+4}">${escape(row.name.slice(0,28))}<tspan class="cf-val"> ${unit(row.weight)} · ${pct}%</tspan></text>`;
      y=end+6;
    }
    return `<h3>Where the ${metric==='cost'?'known estimated cost':'recorded output'} went</h3><p>${agents.length} identities on this page · ${whole?(wholeValue==null?'Whole-session total unavailable':unit(total)+' whole-session known total'):(known.length?unit(total)+' known page total':'Known page total unavailable')}. ${whole?(top?'Top contributors are ranked across the whole session. The remainder includes other identities or missing attribution.':'Shares use the whole-session total; the remainder is outside this page or lacks agent attribution.'):'Shares are of this page only, not the whole session.'} Child-source sessions remain separate.</p>${metric==='cost'?'<p>'+ (rows.length-known.length)+' identities have no current cost estimate. Known estimates may cover only part of their usage; unpriced tokens are not free.</p>':''}${drawn.length?`<div class="cf-wrap"><svg role="img" aria-label="${metric==='cost'?'Known estimated cost':'Recorded output tokens'} by identity ${top?'across the whole session':'on this page'}" viewBox="0 0 980 ${H}" width="980" height="${H}">${svg}</svg></div>`:'<p>No positive known weight on this page. Zero and unknown remain distinct in the details below.</p>'}<table><thead><tr><th>Identity</th><th>Output tokens</th><th>Known cost estimate</th><th>Pricing coverage</th></tr></thead><tbody>${agents.map((a,i)=>`<tr><td>${escape(rows[i].name)}</td><td>${a.tokensOut}</td><td>${a.costEstimate==null?'unpriced':money(a.costEstimate)}</td><td>${a.pricing?escape(a.pricing.pricedTokens)+' / '+escape(a.pricing.recordedTokens)+' recorded tokens priced':'No current pricing coverage'}</td></tr>`).join('')}</tbody></table>`;
  }
  function waterfallMarkup(spans) {
    if(!Array.isArray(spans)||spans.length>100)throw new Error('Invalid waterfall page.');
    const groups=new Map(),times=[];
    const timestamp=value=>typeof value==='string'&&!value.startsWith('0001-')?Date.parse(value):NaN;
    for(const span of spans){
      if(!span?.call||!Number.isSafeInteger(span.call.sequence)||span.call.sequence<1)throw new Error('Invalid waterfall record.');
      const id=span.call.agentId||'';if(!groups.has(id))groups.set(id,[]);groups.get(id).push(span);
      const start=timestamp(span.call.timestamp);if(Number.isFinite(start))times.push(start);
      if(span.state==='matched'){const end=timestamp(span.endedAt);if(!Number.isFinite(start)||!Number.isFinite(end)||end<start||!Number.isSafeInteger(span.durationMillis)||span.durationMillis!==end-start)throw new Error('Invalid waterfall duration.');times.push(end);}
    }
    const low=Math.min(...times),high=Math.max(...times),range=Math.max(1,high-low);
    return `<p>${spans.length} recorded tool calls on this page. Matched result times may be beyond this page. Missing results do not mean a tool is still running. Bars show only matched recorded intervals; a zero-duration mark has a minimum visible width.</p>`+[...groups].sort((a,b)=>a[0]==='main'?-1:b[0]==='main'?1:0).map(([id,rows])=>`<details open><summary class="wf-row wf-agent">${escape(rows[0].call.agentDisplayName||(id==='main'?'Orchestrator':id||'Unattributed'))} · ${rows.length} calls on this page · ${rows.filter(s=>s.state==='matched').length} matched</summary>${rows.map(span=>{
      const call=span.call,start=timestamp(call.timestamp),matched=span.state==='matched',left=Number.isFinite(start)?(start-low)/range*100:0,width=matched?span.durationMillis/range*100:0;
      return `<div class="wf-row wf-span"><button class="wf-name" data-waterfall-record="${call.sequence}">${escape(call.data?.tool||'Unknown tool')} · record ${call.sequence}</button><span class="wf-roll">${matched?span.durationMillis+' ms':escape(span.state)}${span.error===true?' · recorded error':matched&&span.error==null?' · outcome unknown':''}</span><span class="wf-track">${matched?`<span class="wf-bar ${span.error===true?'wf-err':'wf-tool'}" style="left:${left.toFixed(3)}%;width:${width.toFixed(3)}%;min-width:2px" title="${escape(call.timestamp)} — ${escape(span.endedAt)}"></span>`:'Timing unavailable'}</span>${matched?`<button data-waterfall-record="${span.resultSequence}">Inspect result</button>`:''}</div>`;
    }).join('')}</details>`).join('')+(spans.length?'':'<p>No recorded tool calls on this page.</p>');
  }
  function lanesMarkup(events) {
    if(!Array.isArray(events)||events.length>100)throw new Error('Invalid lanes page.');
    if(!events.length)return '<p>No indexed records on this page.</p>';
    const lanes=new Map();
    for(const event of events){
      if(!Number.isSafeInteger(event.sequence)||event.sequence<1)throw new Error('Invalid lane event.');
      const id=event.agentId||'';
      if(!lanes.has(id))lanes.set(id,[]);
      lanes.get(id).push(event);
    }
    const ordered=[...lanes].sort((a,b)=>a[0]==='main'?-1:b[0]==='main'?1:0);
    return `<p>${events.length} indexed records across ${lanes.size} identities on this page. All page records are shown; use page navigation for complete history. Names are current organization labels, not evidence of active work. Card text is an indexed preview; inspect records for source evidence.</p>`+ordered.map(([id,rows])=>{
      const name=rows.find(e=>e.agentDisplayName)?.agentDisplayName||(id==='main'?'Orchestrator':id||'Unattributed');
      return `<section class="lane"><div class="lane-head ${id==='main'?'lane-main':''}"><span class="lane-name">${escape(name)}</span><span class="lane-sub">${rows.length} records on this page</span></div><div class="lane-cards">${rows.map(e=>`<button class="lcard k-${['user-text','user-queued','assistant-text','tool-call','tool-result','spawn','spawn-result'].includes(e.kind)?e.kind:'source-record'}${e.data?.error===true||e.kind==='indexing-error'?' err':''}" data-timeline-sequence="${e.sequence}" title="${escape(e.timestamp||'Timestamp unavailable')} · record ${e.sequence}"><span class="lc-top">${escape(e.data?.tool||e.kind)}</span><span class="lc-txt" style="display:block">${escape(e.text||'No indexed text.')}</span></button>`).join('')}</div></section>`;
    }).join('');
  }
  function timelineMarkup(events) {
    if (!Array.isArray(events) || events.length > 100) throw new Error('Invalid timeline page.');
    const lanes = new Map(), dated = [];
    for (const event of events) {
      if (!Number.isSafeInteger(event.sequence) || event.sequence < 1) throw new Error('Invalid timeline event.');
      const lane = event.agentId || 'Unattributed';
      if (!lanes.has(lane)) lanes.set(lane, lanes.size);
      const time = typeof event.timestamp === 'string' && event.timestamp ? Date.parse(event.timestamp) : NaN;
      if (Number.isFinite(time)) dated.push({event, time, lane});
    }
    const times = dated.map(row => row.time), start = Math.min(...times), end = Math.max(...times);
    const height = lanes.size * 44 + 70;
    const svg = dated.length ? `<svg role="img" aria-label="Recorded events by agent and timestamp on this page" viewBox="0 0 1000 ${height}" width="100%" style="min-width:600px">${[...lanes].map(([name,index])=>`<text x="8" y="${index*44+34}" fill="currentColor">${escape(String(name).slice(0,24))}</text><line x1="200" x2="970" y1="${index*44+44}" y2="${index*44+44}" stroke="#59616f"/>`).join('')}${dated.map(({event,time,lane})=>`<circle cx="${(start===end?585:200+(time-start)/(end-start)*770).toFixed(2)}" cy="${lanes.get(lane)*44+28}" r="5" fill="${event.kind==='indexing-error'?'#f87171':'#8fc7ff'}"><title>${escape(event.kind)} · ${escape(event.timestamp)} · record ${event.sequence}</title></circle>`).join('')}<text x="200" y="${height-8}" fill="currentColor">${escape(new Date(start).toISOString())}</text><text x="970" text-anchor="end" y="${height-8}" fill="currentColor">${escape(new Date(end).toISOString())}</text></svg>` : '<p>No usable timestamps on this page.</p>';
    return `<p>One page of recorded history, not the whole session. Marks indicate observations, not inferred task durations or active agents. ${events.length-dated.length} records have no usable timestamp.</p><div style="overflow:auto">${svg}</div><ol>${events.map(event=>`<li><button data-timeline-sequence="${event.sequence}">Record ${event.sequence}: ${escape(event.kind)} · ${escape(event.timestamp||'Timestamp unavailable')} · ${escape(event.agentId||'Unattributed')}</button></li>`).join('')}</ol>`;
  }

  function lineageMarkup(page) {
    const labels={root:'No parent reference recorded',unique:'One matching parent',unresolved:'Parent reference has not been resolved',ambiguous:'Multiple parent candidates; no unique parent selected'};
    const links=rows=>rows.map(r=>`<li><button data-related-session="${escape(r.id)}">${escape(r.title||r.nativeId||r.id)}</button></li>`).join('');
    return `<h3>Recorded session relationships</h3><p>${escape(labels[page.parentResolution]||'Parent resolution unknown')}${page.parentNativeId?': '+escape(page.parentNativeId):''}</p><ul>${links(page.parents)}</ul><h4>Child references</h4><p>References are source evidence, not a count of active agents. Ambiguous native identities may match more than one session.</p><ul>${links(page.children)}</ul>${page.children.length?'':'<p>No child references on this page.</p>'}<button id="v2LineageFirst">First child page</button><button id="v2LineageNext" ${page.nextCursor?'':'disabled'}>Next child page</button>`;
  }

  function searchAnchor(hit) {
    if(!hit||typeof hit.sessionId!=='string'||!hit.sessionId||!Number.isSafeInteger(hit.sequence)||hit.sequence<1||!/^([a-f0-9]{64})$/.test(hit.historySnapshot||''))
      throw new Error('Search result has no valid history binding. Refresh search with the current hub.');
    return {id:hit.sessionId,sequence:hit.sequence,snapshot:hit.historySnapshot};
  }

  function breakdownMarkup(kind,page,stats) {
    const agents=kind==='agents',rows=agents?page.agents:page.models;
    const value=n=>n==null?'unknown':escape(n);
    const pricedRows=rows.filter(r=>r.pricing);
    const coverage=pricedRows.length?`<h4>Price coverage on this page</h4><ul>${pricedRows.map(r=>`<li>${escape((agents?r.id:r.model)||'Unattributed')}: ${value(r.pricing.pricedTokens)}/${value(r.pricing.recordedTokens)} tokens priced; ${value(r.pricing.unpricedTokens)} unpriced; ${value(r.pricing.unattributedTokens)} unattributed. Catalog ${escape(r.pricing.catalogId)}; snapshot ${escape(r.pricing.snapshotId)}.</li>`).join('')}</ul>`:'';
    return coverage+`<h3>Recorded ${agents?'agent':'model'} accounting</h3><p>${value(stats.agentCount)} recorded agent identities (${escape(stats.agentCountBasis||'basis unknown')}); ${value(stats.events)} events; ${value(stats.toolCalls)} tool calls; ${value(stats.indexingErrors)} indexing errors.</p><p>One bounded page of observed usage. These are not live-agent counts. Missing attribution is not redistributed; session cost does not establish a per-row price.</p><div class="table-scroll"><table><thead><tr><th>${agents?'Agent identity':'Model'}</th><th>Attribution</th><th>Input</th><th>Cache read</th><th>Cache write</th><th>Output</th><th>${agents?'Unknown-model tokens':'Observations'}</th><th>Estimate</th></tr></thead><tbody>${rows.map(r=>`<tr><td>${escape((agents?r.id:r.model)||(agents?'Unattributed agent':'Unknown model'))}</td><td>${escape((agents?r.kind:r.attribution)||'unknown')}</td><td>${value(r.tokensIn)}</td><td>${value(r.tokensCache)}</td><td>${value(r.tokensCacheWrite)}</td><td>${value(r.tokensOut)}</td><td>${value(agents?r.unknownModelTokens:r.observations)}</td><td>${r.costEstimate==null?(r.pricing?'Unpriced':'Not attributed'):'~$'+escape(Number(r.costEstimate).toFixed(3))}</td></tr>`).join('')}</tbody></table></div>${rows.length?'':'<p>No recorded rows on this page.</p>'}<button id="v2BreakdownAgents">Agents</button><button id="v2BreakdownModels">Models</button><button id="v2BreakdownFirst">First page</button><button id="v2BreakdownNext" ${page.nextCursor?'':'disabled'}>Next accounting page</button>`;
  }

  function unsavedMarkup(page,checks=[],busy=false,error='') {
    const labels={checking:'Checking…',unknown:'Unknown',missing:'No longer on disk',ignored:'Ignored by Git',tracked:'Tracked by Git','in-history':'Present in reachable history','outside-approved-roots':'Outside approved local roots','unsupported-path':'Recorded path needs resolution','untracked-no-reachable-history':'Untracked; no reachable commit history'};
    const confirmed=checks.filter(c=>c?.state==='untracked-no-reachable-history').length;
    return `<h2>Unsaved Work</h2><p>Local machine: ${escape(page.machineId)}. ${page.indexedSessions} of ${page.totalSessions} sessions have a current, diagnostic-free file-edit index. Includes archived sessions. ${page.totalFiles} candidate paths across indexed history; this page is not a complete filesystem inventory.</p><p>Scope: recorded Edit, Write, MultiEdit and NotebookEdit attempts—not proof the agent created a file. Shell writes and unresolved relative paths are not inferred. Remote machines are not inspected on this PC.</p><p role="status">${busy?'Checking this bounded page…':`${confirmed} files confirmed untracked with no reachable commit history on this page.`} Unknown, unapproved and unchecked files are not clean results.</p>${error?`<p role="alert">${escape(error)}</p>`:''}<button data-unsaved-rescan ${busy||!page.files.length?'disabled':''}>Check this page again</button><div class="table-wrap"><table class="ftable"><thead><tr><th>Recorded file</th><th>Local evidence</th><th>Latest recorded edit</th><th>Session</th></tr></thead><tbody>${page.files.map((f,i)=>{const c=checks[i];return `<tr><td>${escape(f.path)}<div class="dim">${escape(f.project)}</div>${f.workingDirectory?`<div class="dim">Recorded directory: ${escape(f.workingDirectory)}</div>`:''}${c?.path&&c.path!==f.path?`<div class="dim">Checked path: ${escape(c.path)}</div>`:''}</td><td>${escape(labels[c?.state]||'Not checked')}${c?.problem?`<div class="dim">${escape(c.problem)}</div>`:''}<button data-unsaved-check="${i}" ${busy?'disabled':''}>Check file</button></td><td>${escape(f.lastTouched||'Timestamp unavailable')}</td><td><button data-unsaved-session="${i}">${escape(f.sessionId)}</button><div>${f.sessions} sessions touched this path</div></td></tr>`;}).join('')}</tbody></table></div>${page.files.length?'':'<p>No indexed candidate paths in this snapshot. This does not prove there is no unsaved work.</p>'}`;
  }
  function secretsMarkup(page,checks=[],busy=false,error='') {
    const labels={scanned:'Scanned for the listed shapes',unknown:'Unknown','unsupported-path':'Needs path resolution','outside-approved-roots':'Outside approved roots',missing:'Missing',ignored:'Ignored by Git','skipped-noise':'Skipped generated, documentation or test path','too-large':'Over 512 KiB',binary:'Binary file'};
    return `<h2>Secrets</h2><p>A narrow smoke alarm, not a security audit. Recognizes AWS, GitHub, Slack, Stripe live, Google, private-key and signed-token shapes. It will miss other secrets. Matches are not verified credentials.</p><p>${page.indexedSessions} of ${page.totalSessions} sessions have current edit evidence. Includes archived sessions. ${page.totalFiles} candidate paths; only this page is checked. Nothing is changed, rotated or sent to a vendor.</p><p role="status">${busy?'Scanning this bounded page…':`${checks.filter(c=>c?.state==='scanned').length} files scanned on this page.`} Skipped, unknown and unindexed files are not clean results. No matches does not prove a file is safe.</p>${error?`<p role="alert">${escape(error)}</p>`:''}<button data-secrets-scan ${busy||!page.files.length?'disabled':''}>Scan this page again</button><div class="table-wrap"><table class="ftable"><thead><tr><th>Recorded file</th><th>Scan evidence</th><th>Latest recorded edit</th><th>Session</th></tr></thead><tbody>${page.files.map((f,i)=>{const c=checks[i];return `<tr><td>${escape(f.path)}<div>${escape(f.workingDirectory||f.project||'')}</div></td><td>${escape(labels[c?.state]||'Not checked')}${c?.problem?`<div>${escape(c.problem)}</div>`:''}${(c?.findings||[]).map(h=>`<div>${escape(h.kind)} · line ${h.line} · ${escape(h.fragment)}</div>`).join('')}${c?.capped?'<div>Finding limit reached; more may exist.</div>':''}</td><td>${escape(f.lastTouched||'Timestamp unavailable')}</td><td><button data-secrets-copy="${i}">Copy path</button><button data-secrets-session="${i}">${escape(f.sessionId)}</button></td></tr>`;}).join('')}</tbody></table></div>`;
  }
  function flowRolesMarkup(page,busy=false,error='') {
    const head='<h3>Agent-role summary</h3><p>Historical session/agent appearances, grouped by normalized names—not proven delegations or live-agent counts. No reported errors does not establish success. Observed spans are not execution durations.</p>';
    if(!page)return head+(error?`<p role="alert">${escape(error)}</p>`:'<p role="status">Loading role summaries independently…</p>');
    return head+`<p>${page.totalAppearances} appearances · ${page.totalRoles} role labels. Missing prices are unknown, not zero. Costs are published estimates, not bills.</p>${busy?'<p role="status">Loading role page…</p>':''}${error?`<p role="alert">${escape(error)}</p>`:''}<div class="table-wrap"><table class="ftable"><thead><tr><th>Role</th><th>Appearances</th><th>Reported outcomes</th><th>Mean observed span</th><th>Known cost and coverage</th><th>Machines</th><th>Latest example</th></tr></thead><tbody>${page.roles.map((r,i)=>`<tr><td>${escape(r.name)}</td><td>${r.appearances} in ${r.sessions} sessions</td><td>${r.withErrors} with errors · ${r.withoutReportedErrors} without reported errors · ${r.unknown} unknown</td><td>${r.meanObservedMs===null?'Unknown':`${(r.meanObservedMs/1000).toFixed(2)}s across ${r.durationObserved} appearances`}</td><td>${r.knownCost===null?'Unknown':`~$${r.knownCost.toFixed(4)}`}<div>${r.pricedTokens}/${r.recordedTokens} tokens priced · ${r.unpricedTokens} unpriced · ${r.unattributedTokens} unattributed</div><div>${r.withEstimate}/${r.appearances} appearances with an attributed estimate</div></td><td>${r.machines}</td><td><button data-flow-role-session="${i}">${escape(r.exampleSessionId)}</button><div>${escape(r.lastActivity||'Timestamp unavailable')}</div></td></tr>`).join('')}</tbody></table></div>${page.roles.length?'':'<p>No matching named or unnamed non-root appearances in indexed evidence. This is not proof that no delegation occurred.</p>'}`;
  }
  function flowsMarkup(page,organization=()=>({}),archived=false,error='',rolesMarkup=flowRolesMarkup(null)) {
    const fans={solo:'One observed agent identity','small-team':'2–5 observed agent identities',team:'6–30 observed agent identities',fleet:'More than 30 observed agent identities',unobserved:'No observed agent identity','incomplete-attribution':'Incomplete agent attribution'};
    const outcomes={'reported-errors':'Reported errors',unknown:'Unknown outcome evidence','no-indexed-events':'No indexed events','no-reported-errors':'No reported errors (not proof of success)'};
    return `<h2>How your fleet behaves</h2><p>${page.totalSessions} matching historical sessions · ${page.totalPatterns} patterns. Patterns use each session’s current indexed evidence, not a recent-session sample. Team size is recorded identities, not simultaneous live agents.</p><p>Groups with multiple sessions are recurring patterns. A one-off pattern is worth a look, not proof that something went wrong. Unknown and incomplete history are not clean runs.</p>${error?`<p role="alert">${escape(error)}</p>`:''}<div class="flows-panel"><h3>Patterns and one-off sessions</h3><table class="ftable"><thead><tr><th>Pattern</th><th>Sessions</th><th>Evidence</th><th>Latest example</th></tr></thead><tbody>${page.patterns.map((p,i)=>{const m=organization(p.exampleSessionId);return `<tr><td>${escape(fans[p.fanout])}<div>${escape(p.tools.join(' + ')||'No indexed tool calls')}</div></td><td>${p.sessions}${p.sessions===1?' · one-off':''}</td><td>${escape(outcomes[p.outcome])}</td><td><button data-flow-session="${i}">${escape(p.exampleSessionId)}</button><div>${escape(p.lastActivity||'Timestamp unavailable')}</div>${p.sessions===1&&archived===false?`<button data-flow-archive="${i}" ${m.pending?'disabled':''}>${m.pending?'Archive pending':'Archive'}</button>${m.pending?`<span role="status">${escape(m.pendingState||'pending')}</span>`:''}`:''}</td></tr>`;}).join('')}</tbody></table>${page.patterns.length?'':'<p>No matching indexed patterns. This does not establish that no work occurred.</p>'}</div><section>${rolesMarkup}</section>`;
  }
  function boot({ panes, initialView='table', home=null, notify=null }) {
    const $ = id => document.getElementById(id);
    const pane = $('tableView');
    let client, model, hooksModel, dejaModel, divergenceModel, projectsModel, galaxyModel, fingerprintsModel, ringsModel, usageModel, calendarModel, rhythmModel, brainDraft, live, current = 'table', searchTimer, detailTicket = 0, detailReturnView = 'table';
    let collectorAlerts,budgetAlerts,sessionAlerts,releaseAlerts,runAlerts;
    let storyTimer=null,storyPlayEpoch=0,storyPlaying=false,storySpeed=4;
    document.addEventListener?.('visibilitychange',()=>collectorAlerts?.visibilityChanged());
    window.addEventListener('pagehide',()=>{collectorAlerts?.close();budgetAlerts?.close();sessionAlerts?.close();releaseAlerts?.close();runAlerts?.close();});
    function stopStory(){
      ++storyPlayEpoch;clearTimeout(storyTimer);storyTimer=null;storyPlaying=false;
      if(current==='story'||current==='lanes'){$('v2StoryPlay')&&($('v2StoryPlay').disabled=false);$('v2StoryPause')&&($('v2StoryPause').disabled=true);}
    }
    document.addEventListener?.('visibilitychange',()=>{if(document.hidden)stopStory();});
    if(['machines','divergence','hookprops','flows','leaks','unsaved','trouble','graveyard','dejavu','table','rings','fingerprints','constellation','projects','fleet','calendar','rhythm','usage','economics','brain','audit','playbooks'].includes(initialView))current=initialView;
    let selectedSession = null, exportSession = null;
    let undoController;
	let undoProject=null;
    function selectingSession(id){
      undoController?.abort();
      selectedSession=id;exportSession=null;
      $('exportBtn').disabled=true;
      $('exportBtn').title='Waiting for the selected session before export.';
    }
    function readySessionExport(row,id){
      if(row.id!==id)throw new Error('The returned session identity does not match the selected session.');
      exportSession=id;
      $('exportBtn').disabled=false;
      $('exportBtn').title='Download replay ZIP for '+(row.metadata.name||row.title||id)+'. Includes private indexed history and complete captured source; extract and open replay.html.';
    }
    function renderRings(state){
      if(disposed||current!=='rings'||!state)return;pane.innerHTML=banner+ringsMarkup(state);
      if(home){pane.querySelector('.fleet-head').insertAdjacentHTML('beforeend',home.markup('rings'));home.wire(pane,'rings',()=>renderRings(state));}
      $('v2RingsRefresh').onclick=()=>state.load(true);$('v2RingsMetric').onchange=e=>{state.metric=e.target.value;state.emit();};
      $('v2RingsBack').onclick=()=>state.page('back');$('v2RingsNext').onclick=()=>state.page('next');
      pane.querySelectorAll('[data-ring-project]').forEach(el=>el.onclick=()=>{state.pause();current='table';detailTicket++;model.filter({project:el.dataset.ringProject,unassignedProject:!el.dataset.ringProject,provider:'',machineId:'',q:'',archived:false});});
    }
    function renderFingerprints(state){
      if(!disposed&&current==='constellation'&&state){
        pane.innerHTML=banner+galaxyMarkup(state);
        pane.querySelector('.fleet-head').insertAdjacentHTML('afterend',galaxyFilters(state));
        $('v2GalaxyFilters').onsubmit=e=>{e.preventDefault();const f=e.currentTarget.elements;state.filter({q:f.q.value,provider:f.provider.value,machineId:f.machineId.value,project:f.project.value,archived:f.archived.value==='all'?undefined:f.archived.value==='true'});};
        $('v2GalaxyClear').onclick=()=>state.filter({q:'',provider:'',machineId:'',project:'',archived:false});
        if(home){pane.querySelector('.fleet-head').insertAdjacentHTML('beforeend',home.markup('constellation'));home.wire(pane,'constellation',()=>renderFingerprints(state));}
        $('v2GalaxyRefresh').onclick=()=>state.load();
        $('v2GalaxyBack').onclick=()=>state.page('back');$('v2GalaxyNext').onclick=()=>state.page('next');
        const reset=wireGalaxy($('v2Galaxy'),id=>detail(id),state.galaxyView);
        $('v2GalaxyReset').onclick=()=>{reset();state.galaxyView.positions.clear();state.emit();};
        return;
      }
      if(disposed||current!=='fingerprints'||!state)return;
      pane.innerHTML=banner+fingerprintsMarkup(state);
      if(home){pane.querySelector('.fleet-head').insertAdjacentHTML('beforeend',home.markup('fingerprints'));home.wire(pane,'fingerprints',()=>renderFingerprints(state));}
      $('v2FingerprintFilters').onsubmit=e=>{e.preventDefault();const f=e.currentTarget.elements;state.filter({q:f.q.value,provider:f.provider.value,machineId:f.machineId.value,project:f.project.value,archived:f.archived.value==='all'?undefined:f.archived.value==='true'});};
      $('v2FingerprintRefresh').onclick=()=>state.load();
      $('v2FingerprintSize').onchange=e=>{state.size=e.target.value;state.emit();};
      $('v2FingerprintBack').onclick=()=>state.page('back');$('v2FingerprintNext').onclick=()=>state.page('next');
      pane.querySelectorAll('[data-fingerprint-open]').forEach(el=>el.onclick=()=>detail(el.dataset.fingerprintOpen));
    }
    function renderHooks(state){
      if(disposed||current!=='hookprops'||!state)return;
      pane.innerHTML=banner+hookIdeasMarkup(state)+(state.secret?globalThis.AMCV2SecretHook.markup(state.secret):'');
      if(state.secret)globalThis.AMCV2SecretHook.wire(pane,state.secret,{copy:text=>navigator.clipboard.writeText(text),isActive:()=>!disposed&&current==='hookprops'});
      if(home){pane.querySelector('.fleet-head').insertAdjacentHTML('beforeend',home.markup('hookprops'));home.wire(pane,'hookprops',()=>renderHooks(state));}
      $('v2HooksRefresh').onclick=()=>state.load();
      const copy=$('v2HooksCopy');if(copy)copy.onclick=async()=>{const ticket=state.ticket;try{await navigator.clipboard.writeText(javascriptHookJSON());if(current==='hookprops'&&ticket===state.ticket)copy.textContent='Copied — nothing installed';}catch(error){if(current==='hookprops'&&ticket===state.ticket){state.error='Clipboard unavailable: '+error.message;state.emit();}}};
      const undoCopy=$('v2UndoHookCopy');if(undoCopy)undoCopy.onclick=async()=>{const ticket=state.ticket;try{await navigator.clipboard.writeText(undoHookJSON());if(current==='hookprops'&&ticket===state.ticket)undoCopy.textContent='Copied — nothing installed';}catch(error){if(current==='hookprops'&&ticket===state.ticket){state.error='Clipboard unavailable: '+error.message;state.emit();}}};
      const undoEvidence=$('v2UndoHookEvidence');if(undoEvidence)undoEvidence.onclick=()=>{state.pause();fleetUndoHistory();};
      const modelCopy=$('v2ModelHookCopy');if(modelCopy)modelCopy.onclick=async()=>{if(disposed||current!=='hookprops'||state.loading||state.error)return;const ticket=state.ticket;try{await navigator.clipboard.writeText(modelHookJSON());if(!disposed&&current==='hookprops'&&ticket===state.ticket)modelCopy.textContent='Copied — nothing installed';}catch(error){if(!disposed&&current==='hookprops'&&ticket===state.ticket){state.error='Clipboard unavailable: '+error.message;state.emit();}}};
      const brainButton=$('v2HooksBrain');if(brainButton)brainButton.onclick=()=>{state.pause();current='brain';detailTicket++;renderBrain();brain();};
      pane.querySelectorAll('[data-hook-session]').forEach(button=>button.onclick=()=>detail(button.dataset.hookSession));
    }
    function renderDeja(state){
      if(disposed||current!=='dejavu'||!state)return;
      pane.innerHTML=banner+dejaMarkup(state);
      if(home){pane.querySelector('.fleet-head').insertAdjacentHTML('beforeend',home.markup('dejavu'));home.wire(pane,'dejavu',()=>renderDeja(state));}
      $('v2DejaForm').onsubmit=e=>{e.preventDefault();const f=e.currentTarget.elements;state.search(f.text.value,true,f.mode.value==='related');};
      $('v2DejaFirst').onclick=()=>state.search();$('v2DejaBack').onclick=()=>state.page('back');$('v2DejaNext').onclick=()=>state.page('next');
      pane.querySelectorAll('[data-deja-open]').forEach(el=>el.onclick=()=>{try{const hit=searchAnchor(state.rows[Number(el.dataset.dejaOpen)]);detail(hit.id,0,hit.snapshot,hit.sequence);}catch(error){state.error=error.message;state.emit();}});
      pane.querySelectorAll('[data-deja-open]').forEach(el=>{
        const compare=document.createElement('button');compare.textContent='Find same-instruction runs';
        compare.onclick=()=>{try{const hit=searchAnchor(state.rows[Number(el.dataset.dejaOpen)]);state.pause();detailTicket++;current='divergence';divergenceModel.open(hit.id,hit.sequence,hit.snapshot);}catch(error){state.error=error.message;state.emit();}};
        el.insertAdjacentElement('afterend',compare);
      });
    }
    function renderDivergence(state){
      if(disposed||current!=='divergence'||!state)return;
      pane.innerHTML=banner+divergenceMarkup(state);
      if(home){pane.querySelector('.fleet-head').insertAdjacentHTML('beforeend',home.markup('divergence'));home.wire(pane,'divergence',()=>renderDivergence(state));}
      const refresh=$('v2DivRefresh');if(refresh)refresh.onclick=()=>state.open(state.id,state.anchor);
      const back=$('v2DivBack');if(back)back.onclick=()=>state.page('back');
      const next=$('v2DivNext');if(next)next.onclick=()=>state.page('next');
      pane.querySelectorAll('[data-div-child]').forEach(el=>el.onclick=()=>state.resolve(Number(el.dataset.divChild)));
      pane.querySelectorAll('[data-div-compare]').forEach(el=>el.onclick=()=>state.compare(Number(el.dataset.divCompare)));
      const continueTrace=$('v2DivContinueTrace');if(continueTrace)continueTrace.onclick=()=>state.continueComparison();
      pane.querySelectorAll('[data-div-call]').forEach(el=>el.onclick=()=>{const ref=state.pageData.matches[Number(el.dataset.divCall)];state.pause();detail(ref.sessionId,0,ref.sessionSnapshot,ref.sequence);});
      const child=$('v2DivOpenChild');if(child)child.onclick=()=>{const ref=state.child;state.pause();detail(ref.childSessionId,0,ref.childSnapshot);};
    }
    function renderProjects(state){
      if(disposed||current!=='projects'||!state)return;
      pane.innerHTML=banner+projectsMarkup(state);
      $('v2ProjectNew').onclick=()=>editProject({name:'',color:'#60a5fa'});
      $('v2ProjectRefresh').onclick=()=>state.load();$('v2ProjectsBack').onclick=()=>state.page('back');$('v2ProjectsNext').onclick=()=>state.page('next');
      pane.querySelectorAll('[data-project-edit]').forEach(el=>el.onclick=()=>editProject(state.columns.find(c=>c.project.id===el.dataset.projectEdit).project));
      pane.querySelectorAll('[data-project-retry]').forEach(el=>el.onclick=()=>state.submit(state.pending[Number(el.dataset.projectRetry)]));
      pane.querySelectorAll('[data-project-open]').forEach(el=>el.onclick=()=>detail(el.dataset.projectOpen));
      pane.querySelectorAll('[data-project-next]').forEach(el=>el.onclick=()=>state.pageColumn(el.dataset.projectNext,'next'));
      pane.querySelectorAll('[data-project-back]').forEach(el=>el.onclick=()=>state.pageColumn(el.dataset.projectBack,'back'));
      pane.querySelectorAll('[data-project-move]').forEach(el=>el.onchange=()=>state.move(el.dataset.projectMove,el.value));
      pane.querySelectorAll('[data-project-picker]').forEach(el=>el.onclick=()=>pickProject(el.dataset.projectPicker));
      pane.querySelectorAll('[data-project-session]').forEach(el=>el.ondragstart=e=>{if(state.moves.has(el.dataset.projectSession)){e.preventDefault();return;}e.dataTransfer.setData('application/x-amc-session',el.dataset.projectSession);});
      pane.querySelectorAll('[data-project-drop]').forEach(el=>{el.ondragover=e=>{e.preventDefault();el.classList.add('drag-over');};el.ondragleave=()=>el.classList.remove('drag-over');el.ondrop=e=>{e.preventDefault();el.classList.remove('drag-over');state.move(e.dataTransfer.getData('application/x-amc-session'),el.dataset.projectDrop);};});
      pane.querySelectorAll('[data-project-audit]').forEach(el=>el.onclick=()=>projectAudit(el.dataset.projectAudit));
    }
    function pickProject(sessionID){
      const dialog=document.createElement('dialog');document.body.appendChild(dialog);
      const picker=new ProjectPicker(client,state=>{
        dialog.innerHTML=`<h3>Choose destination project</h3><p>Move this session directly to any project. Only this destination page is loaded.</p><p role="status">${escape(state.error||(state.loading?'Loading projects…':''))}</p><button data-destination="" ${state.loading?'disabled':''}>Move to Unassigned</button><ul>${state.items.map(p=>`<li><button data-destination="${escape(p.id)}" ${state.loading?'disabled':''}>Move to ${escape(p.name)}</button></li>`).join('')}</ul><button data-picker-back ${state.loading||!state.previous.length?'disabled':''}>Previous destinations</button><button data-picker-next ${state.loading||!state.nextCursor?'disabled':''}>Next destinations</button><button data-picker-refresh ${state.loading?'disabled':''}>Refresh destinations</button><button data-picker-close>Cancel move</button>`;
        dialog.querySelectorAll('[data-destination]').forEach(el=>el.onclick=()=>{const id=el.dataset.destination,selected=state.items.find(p=>p.id===id);if(id&&!selected)return;projectsModel.move(sessionID,id,selected?.name||'Unassigned');dialog.close();});
        dialog.querySelector('[data-picker-back]').onclick=()=>state.page('back');dialog.querySelector('[data-picker-next]').onclick=()=>state.page('next');dialog.querySelector('[data-picker-refresh]').onclick=()=>state.load();dialog.querySelector('[data-picker-close]').onclick=()=>dialog.close();
      });
      dialog.addEventListener('close',()=>{picker.close();dialog.remove();});dialog.showModal();picker.load();
    }
    function editProject(project){
      const dialog=document.createElement('dialog');dialog.innerHTML=`<form><h3>${project.id?'Edit project':'New project'}</h3><label>Project name<input name="name" required maxlength="1024" value="${escape(project.name)}"></label><label>Project color<input name="color" type="color" value="${escape(project.color)}"></label><p role="status"></p><button type="submit">Save project</button>${project.id?'<button type="button" data-delete>Delete project</button>':''}<button type="button" data-close>Close editor</button></form>`;
      document.body.appendChild(dialog);dialog.addEventListener('close',()=>dialog.remove());dialog.querySelector('[data-close]').onclick=()=>dialog.close();
      const form=dialog.querySelector('form');
      async function submit(remove){
        if(remove&&!window.confirm('Delete this project? All its sessions, including archived sessions, become unassigned. Chats and other metadata are preserved.'))return;
        const controls=[...dialog.querySelectorAll('input,button:not([data-close])')];controls.forEach(el=>el.disabled=true);
        dialog.querySelector('[role="status"]').textContent=remove?'Deletion pending. Memberships are being cleared in batches; chats and other metadata are preserved. You can close this editor.':'Saving project…';
        const ok=await projectsModel.save({...project,name:form.elements.name.value,color:form.elements.color.value},remove);
        if(ok){dialog.close();return;}
        dialog.querySelector('[role="status"]').textContent=projectsModel.error+' Close this editor to retry any saved operation or refresh after a conflict.';
        if(!projectsModel.pending.length)controls.forEach(el=>el.disabled=false);
      }
      form.onsubmit=e=>{e.preventDefault();submit(false);};if(project.id)dialog.querySelector('[data-delete]').onclick=()=>submit(true);dialog.showModal();
    }
    async function projectAudit(id,after=0){
      const ticket=detailTicket,target=$('v2ProjectAudit');if(!target)return;target.textContent='Loading project history…';
      try{const page=await client.projectHistory(id,{after,limit:20});if(disposed||current!=='projects'||ticket!==detailTicket||target!==$('v2ProjectAudit'))return;
        target.innerHTML='<h3>Project history</h3>'+page.items.map(a=>`<details><summary>Revision ${a.revision} · ${escape(a.at)}${a.after.deleted?' · deleted':''}</summary><pre>${escape(JSON.stringify({before:a.before,after:a.after,request:a.request},null,2))}</pre></details>`).join('')+`<button id="v2ProjectAuditFirst">First audit page</button><button id="v2ProjectAuditNext" ${page.next?'':'disabled'}>Next audit page</button>`;
        $('v2ProjectAuditFirst').onclick=()=>projectAudit(id);$('v2ProjectAuditNext').onclick=()=>projectAudit(id,page.next);
      }catch(e){if(target===$('v2ProjectAudit'))target.textContent=e.message;}
    }
    let brainItems=[],brainTicket=0;
    let auditTicket=0;
    let ordersTicket=0;
    let economicsTicket=0,economicsQuery={};
    async function economics(){
      if(!client)return;const ticket=++economicsTicket;
      pane.innerHTML=banner+'<h2>Economics</h2><label>Economics provider <select id="v2EconProvider"><option value="">All providers</option><option>claude</option><option>codex</option><option>otel</option></select></label><label>Economics project <input id="v2EconProject"></label><label>Economics machine <input id="v2EconMachine"></label><label>Economics visibility <select id="v2EconArchive"><option value="all">All history</option><option value="false">Active</option><option value="true">Archived</option></select></label><button id="v2EconRefresh">Apply filters / refresh costs</button><div id="v2EconResult" role="status">Loading published estimates…</div>';
      $('v2EconProvider').value=economicsQuery.provider||'';$('v2EconProject').value=economicsQuery.project||'';$('v2EconMachine').value=economicsQuery.machineId||'';$('v2EconArchive').value=economicsQuery.archived==null?'all':String(economicsQuery.archived);
      $('v2EconRefresh').onclick=()=>{economicsQuery={provider:$('v2EconProvider').value,project:$('v2EconProject').value,machineId:$('v2EconMachine').value,archived:$('v2EconArchive').value==='all'?undefined:$('v2EconArchive').value==='true'};economics();};
      $('v2EconResult').innerHTML='<section id="v2EconCosts">Loading published costs…</section><section id="v2EconLifetimes">Loading observed lifetimes…</section><section id="v2EconModels">Loading recorded models…</section><section id="v2EconHistory">Loading fleet history…</section><section id="v2EconResolutions">Loading capture audit…</section><section id="v2EconLegacy">Loading preserved legacy evidence…</section>';
      const query={...economicsQuery};
      let modelRead=0;
      async function models(cursor='',previous=[]){
        const read=++modelRead;
        const active=()=>!disposed&&current==='economics'&&ticket===economicsTicket&&read===modelRead;
        if(!active())return;
        $('v2EconModels').textContent='Loading recorded models…';
        try{
          const page=await client.usageGroups('model',{...query,cursor,limit:100});
          if(!active())return;
          $('v2EconModels').innerHTML=economicsModelsMarkup(page)+'<button id="v2EconModelsFirst">First models</button><button id="v2EconModelsPrevious">Previous models</button><button id="v2EconModelsNext">Next models</button>';
          $('v2EconModelsFirst').disabled=!cursor;$('v2EconModelsPrevious').disabled=!previous.length;$('v2EconModelsNext').disabled=!page.nextCursor;
          $('v2EconModelsFirst').onclick=()=>models();
          $('v2EconModelsPrevious').onclick=()=>models(previous[previous.length-1],previous.slice(0,-1));
          $('v2EconModelsNext').onclick=()=>models(page.nextCursor,[...previous,cursor].slice(-20));
        }catch(error){if(active()){
          $('v2EconModels').textContent=error.message;
          const retry=document.createElement('button');retry.textContent='Retry model page';retry.onclick=()=>models(cursor,previous);$('v2EconModels').appendChild(retry);
        }}
      }
      const modelWork=models();
      const captureButton=document.createElement('button'),captureStatus=document.createElement('span');
      captureButton.textContent='Capture whole-fleet measurement now';
      $('v2EconResult').appendChild(captureButton);$('v2EconResult').appendChild(captureStatus);
      try{if(client.pendingEconomicsCapture())captureButton.textContent='Retry pending fleet capture';}catch(error){captureStatus.textContent=error.message;captureButton.disabled=true;}
      const resolveButton=document.createElement('button');resolveButton.textContent='Review unavailable capture';resolveButton.hidden=true;$('v2EconResult').appendChild(resolveButton);
      let resolutionConfirmed=false,reviewedCaptureID=null;
      resolveButton.onclick=async()=>{
        if(!resolutionConfirmed){try{reviewedCaptureID=client.pendingEconomicsCapture();}catch(error){captureStatus.textContent=error.message;return;}resolutionConfirmed=true;captureStatus.textContent='This records that the original capture is unavailable, without claiming it ever completed. The audit record is preserved. No replacement measurement will be taken.';resolveButton.textContent='Confirm: record unavailable attempt';return;}
        resolveButton.disabled=true;captureButton.disabled=true;
        try{const resolution=await client.resolvePendingEconomicsCapture(reviewedCaptureID);if(!disposed&&current==='economics'&&ticket===economicsTicket){captureStatus.textContent='Unavailable attempt recorded: '+resolution.id+'. A new measurement is a separate action.';resolveButton.hidden=true;captureButton.textContent=client.pendingEconomicsCapture()?'Retry another pending fleet capture':'Capture whole-fleet measurement now';await resolutions();}}
        catch(error){if(!disposed&&current==='economics'&&ticket===economicsTicket){captureStatus.textContent=error.message;resolutionConfirmed=false;resolveButton.textContent='Review unavailable capture';}}
        finally{if(!disposed&&current==='economics'&&ticket===economicsTicket){resolveButton.disabled=false;captureButton.disabled=false;}}
      };
      captureButton.onclick=async()=>{
        resolutionConfirmed=false;resolveButton.hidden=true;resolveButton.textContent='Review unavailable capture';
        captureButton.disabled=true;captureStatus.textContent='Capturing whole-fleet evidence…';
        try{await client.captureEconomics();if(!disposed&&current==='economics'&&ticket===economicsTicket){captureStatus.textContent='Snapshot recorded.';captureButton.textContent=client.pendingEconomicsCapture()?'Retry another pending fleet capture':'Capture whole-fleet measurement now';await history();}}
        catch(error){if(!disposed&&current==='economics'&&ticket===economicsTicket){captureStatus.textContent=error.message;captureButton.textContent='Retry pending fleet capture';resolveButton.hidden=error.code!=='capture_reconciliation_required';}}
        finally{if(!disposed&&current==='economics'&&ticket===economicsTicket)captureButton.disabled=false;}
      };
      let historyRead=0;
      async function history(cursor='',previous=[]){
        const read=++historyRead,active=()=>!disposed&&current==='economics'&&ticket===economicsTicket&&read===historyRead;
        if(!active())return;$('v2EconHistory').textContent='Loading fleet history…';
        try{
          const page=await client.economicsHistory({cursor,limit:100});if(!active())return;
          $('v2EconHistory').innerHTML=economicsHistoryCharts(page)+economicsHistoryMarkup(page)+'<button id="v2EconHistoryFirst">Latest measurements</button><button id="v2EconHistoryPrevious">Newer measurements</button><button id="v2EconHistoryNext">Older measurements</button>';
          $('v2EconHistoryFirst').disabled=!cursor;$('v2EconHistoryPrevious').disabled=!previous.length;$('v2EconHistoryNext').disabled=!page.nextCursor;
          $('v2EconHistoryFirst').onclick=()=>history();$('v2EconHistoryPrevious').onclick=()=>history(previous[previous.length-1],previous.slice(0,-1));$('v2EconHistoryNext').onclick=()=>history(page.nextCursor,[...previous,cursor].slice(-20));
        }catch(error){if(active()){$('v2EconHistory').textContent=error.message;const retry=document.createElement('button');retry.textContent='Retry history page';retry.onclick=()=>history(cursor,previous);$('v2EconHistory').appendChild(retry);}}
      }
      let resolutionRead=0;
      async function resolutions(cursor='',previous=[]){
        const read=++resolutionRead,active=()=>!disposed&&current==='economics'&&ticket===economicsTicket&&read===resolutionRead;
        if(!active())return;$('v2EconResolutions').textContent='Loading capture audit…';
        try{
          const page=await client.economicsCaptureResolutions({cursor,limit:100});if(!active())return;
          $('v2EconResolutions').innerHTML=economicsResolutionMarkup(page)+'<button id="v2EconResolutionFirst">Latest audit records</button><button id="v2EconResolutionPrevious">Newer audit records</button><button id="v2EconResolutionNext">Older audit records</button>';
          $('v2EconResolutionFirst').disabled=!cursor;$('v2EconResolutionPrevious').disabled=!previous.length;$('v2EconResolutionNext').disabled=!page.nextCursor;
          $('v2EconResolutionFirst').onclick=()=>resolutions();$('v2EconResolutionPrevious').onclick=()=>resolutions(previous[previous.length-1],previous.slice(0,-1));$('v2EconResolutionNext').onclick=()=>resolutions(page.nextCursor,[...previous,cursor].slice(-20));
        }catch(error){if(active()){$('v2EconResolutions').textContent=error.message;const retry=document.createElement('button');retry.textContent='Reload capture audit';retry.onclick=()=>resolutions();$('v2EconResolutions').appendChild(retry);}}
      }
      let legacyRead=0;
      async function legacy(cursor='',previous=[]){
        const read=++legacyRead,active=()=>!disposed&&current==='economics'&&ticket===economicsTicket&&read===legacyRead;
        if(!active())return;$('v2EconLegacy').textContent='Loading preserved legacy evidence…';
        try{
          const page=await client.legacyEconomics({cursor,limit:100});if(!active())return;
          $('v2EconLegacy').innerHTML=legacyEconomicsMarkup(page)+'<button id="v2EconLegacyFirst">First legacy records</button><button id="v2EconLegacyPrevious">Previous legacy records</button><button id="v2EconLegacyNext">Next legacy records</button>';
          $('v2EconLegacyFirst').disabled=!cursor;$('v2EconLegacyPrevious').disabled=!previous.length;$('v2EconLegacyNext').disabled=!page.nextCursor;
          $('v2EconLegacyFirst').onclick=()=>legacy();$('v2EconLegacyPrevious').onclick=()=>legacy(previous[previous.length-1],previous.slice(0,-1));$('v2EconLegacyNext').onclick=()=>legacy(page.nextCursor,[...previous,cursor].slice(-20));
        }catch(error){if(active()){$('v2EconLegacy').innerHTML=legacyEconomicsIntro;const problem=document.createElement('p');problem.textContent=error.message;$('v2EconLegacy').appendChild(problem);const retry=document.createElement('button');retry.textContent='Retry legacy page';retry.onclick=()=>legacy(cursor,previous);$('v2EconLegacy').appendChild(retry);}}
      }
      await Promise.all([modelWork,history(),resolutions(),legacy(),...[['v2EconCosts',()=>client.economicsCosts(query),economicsCostMarkup],['v2EconLifetimes',()=>client.economicsLifetimes(query),economicsLifetimeMarkup]].map(async([target,read,render])=>{
        try{const result=await read();if(!disposed&&current==='economics'&&ticket===economicsTicket)$(target).innerHTML=render(result);}
        catch(error){if(!disposed&&current==='economics'&&ticket===economicsTicket)$(target).textContent=error.message;}
      })]);
    }
    let plantDraft={title:'',body:'',topic:'general',reviewEveryDays:30,targets:[]},plantPending=null,plantBusy=false;
    let bookPage={items:[]},bookAfter='',bookTicket=0,bookDraft=null,bookBaseline='',bookPending=null,bookSaving=false,bookError='';
    const bookDirty=()=>!!bookDraft&&JSON.stringify(bookDraft)!==bookBaseline;
    function recoverBookOperation(){
      const pending=client.pendingPlaybookOperations();
      if(!pending.length)return false;
      bookPending=pending[0];bookDraft={id:bookPending.id,revision:bookPending.expectedRevision,name:bookPending.name||'Pending deletion',body:bookPending.body||'',kind:bookPending.kind||'custom'};bookBaseline='';
      bookError=`${pending.length} unresolved Playbook operation(s) recovered. Retry the same operation to reconcile its result.`;
      return true;
    }
    let fleetTimer,fleetFlight=null,fleetCursor='',disposed=false;
    const hideOtherPanes = () => {
      stopStory();
      // Main navigation starts a new history route; detail paging does not.
      detailReturnView='table';
      for (const id of panes) if ($(id)) $(id).style.display = id === 'tableView' ? '' : 'none';
      document.querySelector('main').classList.add('no-feed');
      document.querySelector('footer').style.display = 'none';
      for (const id of ['feed','statbar','liveNowStrip','stickbar','failbar','spicker']) $(id).style.display = 'none';
    };
    hideOtherPanes();
    function renderBrain() {
      if(disposed||current!=='brain'||!brainDraft)return;
      const d=brainDraft;
      pane.innerHTML=banner+'<h2>Brain · local guidance files</h2><p>Desktop-owned edits create snapshots and audit records. Saving does not commit or push Git changes.</p>'+
        '<label for="v2BrainFile">Guidance file</label><select id="v2BrainFile"><option value="">Choose a file</option>'+brainItems.map(item=>`<option value="${escape(item.id)}" ${d.file?.id===item.id?'selected':''}>${escape(item.name||item.path||item.id)}</option>`).join('')+'</select>'+
        `<button id="v2BrainInventory">Refresh inventory</button><p id="v2BrainStatus" role="status">${escape(d.error||(d.loading?'Loading file…':d.saving?'Saving…':d.dirty?'Unsaved draft':'No pending edits'))}</p>`+
        (d.file?`<p>${escape(d.file.path||d.file.name||d.file.id)}</p><label for="v2BrainText">File content</label><textarea id="v2BrainText" rows="24" style="width:100%" ${d.loading?'disabled':''}>${escape(d.text)}</textarea><button id="v2BrainSave" ${!d.dirty||d.loading||d.saving||d.unknown?'disabled':''}>Review and save</button><button id="v2BrainCompare" ${d.loading||d.saving?'disabled':''}>Compare disk</button><button id="v2BrainReload" ${d.loading||d.saving?'disabled':''}>Reload file</button><button id="v2BrainHistory">Snapshot history</button><section id="v2BrainEvidence"></section>`:'<p>Select an explicitly registered local guidance file.</p>');
      $('v2BrainFile').disabled=d.loading||d.saving;
      $('v2BrainFile').onchange=e=>{const id=e.target.value;if(id&&(!d.dirty||window.confirm('Discard the current unsaved draft and load this file?')))d.open(id,{discard:true});else renderBrain();};
      $('v2BrainInventory').onclick=()=>brain();
      if(!d.file)return;
      $('v2BrainText').oninput=e=>{d.edit(e.target.value);$('v2BrainSave').disabled=!d.dirty||d.saving||d.unknown;$('v2BrainStatus').textContent=d.error||(d.saving?'Saving…':d.dirty?'Unsaved draft':'No pending edits');$('v2BrainEvidence').replaceChildren();};
      $('v2BrainSave').onclick=()=>{
        const reviewedText=d.text,reviewedFile=d.file;
        $('v2BrainEvidence').innerHTML='<h3>Review file changes</h3><p>Saving retains a snapshot and audit entry. No Git commit or push will run.</p><h4>Loaded content</h4><pre>'+escape(d.file.content)+'</pre><h4>Proposed content</h4><pre>'+escape(reviewedText)+'</pre><button id="v2BrainConfirmSave">Confirm file save</button><button id="v2BrainCancelSave">Cancel review</button>';
        $('v2BrainConfirmSave').onclick=()=>{if(d.file===reviewedFile&&d.text===reviewedText)d.save();else{d.error='The draft changed. Review it again before saving.';renderBrain();}};
        $('v2BrainCancelSave').onclick=()=>{$('v2BrainEvidence').replaceChildren();};
      };
      $('v2BrainReload').onclick=()=>{if(!d.dirty||window.confirm('Discard this draft and reload the file from disk?'))d.open(d.file.id,{discard:true});};
      $('v2BrainCompare').onclick=async()=>{
        const id=d.file.id,ticket=d.ticket,disk=await d.compareDisk();
        if(!disk||disposed||current!=='brain'||d.ticket!==ticket||d.file.id!==id)return;
        const evidence=$('v2BrainEvidence');if(!evidence)return;
        evidence.innerHTML='<h3>Current disk content</h3><p>Your draft and its original conflict hash are unchanged. Copy any draft you need before reloading.</p><pre>'+escape(disk.content)+'</pre>';
      };
      let historyRequest=0;
      const loadBrainHistory=async(before='')=>{
        const request=++historyRequest;
        const target=$('v2BrainEvidence');
        const id=d.file.id,ticket=d.ticket;
        try {
          const result=await client.brainHistory(id,before);
          if(disposed||current!=='brain'||d.ticket!==ticket||d.file.id!==id||request!==historyRequest||target!==$('v2BrainEvidence'))return;
          const evidence=$('v2BrainEvidence');if(!evidence)return;
          evidence.innerHTML='<h3>File snapshots · up to 100 per page</h3><button id="v2BrainHistoryFirst">Newest snapshots</button><button id="v2BrainHistoryNext" '+(result.nextCursor?'':'disabled')+'>Older snapshots</button>'+result.history.map(row=>`<button data-brain-stamp="${escape(row.stamp)}">${escape(row.stamp)} · ${escape(row.size)} bytes</button>`).join('');
          $('v2BrainHistoryFirst').onclick=()=>loadBrainHistory();
          $('v2BrainHistoryNext').onclick=()=>loadBrainHistory(result.nextCursor);
          evidence.querySelectorAll('[data-brain-stamp]').forEach(button=>button.onclick=async()=>{
            try {
              const snapshot=await client.brainSnapshot(id,button.dataset.brainStamp);
              if(disposed||current!=='brain'||d.ticket!==ticket||d.file.id!==id||!button.isConnected)return;
              evidence.innerHTML='<h3>Snapshot content</h3><pre>'+escape(snapshot.content)+'</pre><button id="v2BrainRestoreDraft">Use snapshot as draft</button>';
              $('v2BrainRestoreDraft').onclick=()=>{if(!d.dirty||window.confirm('Replace the current draft with this snapshot?')){if(d.edit(snapshot.content))renderBrain();}};
            }catch(error){if(!disposed&&current==='brain'&&d.ticket===ticket){d.error=error.message;renderBrain();}}
          });
        }catch(error){if(!disposed&&current==='brain'&&d.ticket===ticket){d.error=error.message;renderBrain();}}
      };
      $('v2BrainHistory').onclick=()=>loadBrainHistory();
    }
    async function brain() {
      if(!client)return;
      const ticket=++brainTicket;
      try {
        const result=await client.brain();
        if(disposed||current!=='brain'||ticket!==brainTicket)return;
        brainItems=result.items;renderBrain();
      }catch(error){if(!disposed&&current==='brain'&&ticket===brainTicket){brainDraft.error=error.message;renderBrain();}}
    }
    async function audit(before='') {
      if(!client)return;
      const ticket=++auditTicket;
      pane.innerHTML=banner+'<p role="status">Loading desktop audit history…</p>';
      try {
        const page=await client.desktopAudit(before);
        if(disposed||current!=='audit'||ticket!==auditTicket)return;
        pane.innerHTML=banner+'<h2>Desktop audit history</h2><p>Local file and repository operations · up to 100 records per page. Hub session organization has its own audit history.</p><button id="v2AuditFirst">Newest records</button><button id="v2AuditNext" '+(page.nextCursor?'':'disabled')+'>Older records</button><table><thead><tr><th>ID</th><th>Time</th><th>Operation</th><th>Status</th><th>Path</th><th>Detail</th></tr></thead><tbody>'+page.entries.map(row=>`<tr><td>${escape(row.id)}</td><td>${escape(new Date(row.at).toISOString())}</td><td>${escape(row.kind)}</td><td>${escape(row.status)}</td><td>${escape(row.path)}</td><td><details><summary>Inspect record</summary><pre>${escape(JSON.stringify(row,null,2))}</pre></details></td></tr>`).join('')+'</tbody></table>';
        $('v2AuditFirst').onclick=()=>audit();$('v2AuditNext').onclick=()=>audit(page.nextCursor);
      }catch(error){if(!disposed&&current==='audit'&&ticket===auditTicket){pane.innerHTML=banner+'<p role="alert">'+escape(error.message)+'</p><button id="v2AuditRetry">Retry audit page</button>';$('v2AuditRetry').onclick=()=>audit(before);}}
    }
    function renderPlaybooks(){
      if(disposed||current!=='playbooks')return;
      pane.innerHTML=banner+'<h2>Playbooks</h2><p>Saved guidance only. These controls do not launch agents or execute playbooks.</p><button id="v2OrdersOpen">Standing orders and targets</button><button id="v2BooksFirst">First page</button><button id="v2BooksNext" '+(bookPage.nextCursor?'':'disabled')+'>Next page</button><button id="v2BookNew">New playbook</button><p role="status" id="v2BookStatus">'+escape(bookError||(bookSaving?'Saving…':bookDirty()?'Unsaved draft':''))+'</p>'+bookPage.items.map(item=>`<button data-book-id="${escape(item.id)}">${escape(item.name)} · revision ${escape(item.revision)}</button>`).join('')+
      (bookDraft?`<h3>${bookDraft.id?'Edit playbook':'New playbook'}</h3><label for="v2BookName">Name</label><input id="v2BookName" value="${escape(bookDraft.name)}" ${bookPending?'disabled':''}><label for="v2BookBody">Guidance</label><textarea id="v2BookBody" rows="18" style="width:100%" ${bookPending?'disabled':''}>${escape(bookDraft.body)}</textarea><button id="v2BookSave" ${bookSaving||bookPending?'disabled':''}>Save playbook</button><button id="v2BookDelete" ${!bookDraft.id||bookSaving||bookPending?'disabled':''}>Review delete</button>${bookPending&&!bookSaving?'<button id="v2BookRetry">Retry same operation</button>':''}<section id="v2BookReview"></section>`:'');
      $('v2BooksFirst').onclick=()=>playbooks();$('v2BooksNext').onclick=()=>playbooks(bookPage.nextCursor);
      $('v2OrdersOpen').onclick=()=>{current='orders';standingOrders();};
      const mayReplace=()=>!bookPending&&(!bookDirty()||window.confirm('Discard the current Playbook draft?'));
      $('v2BookNew').onclick=()=>{if(!mayReplace())return;bookTicket++;bookDraft={id:'',revision:0,name:'Untitled play',body:'',kind:'custom'};bookBaseline=JSON.stringify(bookDraft);bookError='';renderPlaybooks();};
      pane.querySelectorAll('[data-book-id]').forEach(button=>button.onclick=async()=>{
        if(!mayReplace())return;const ticket=++bookTicket;bookDraft=null;bookBaseline='';bookError='Loading playbook…';renderPlaybooks();
        try{const item=await client.playbook(button.dataset.bookId);if(disposed||ticket!==bookTicket||bookPending)return;bookDraft={id:item.id,revision:item.revision,name:item.name,body:item.body,kind:item.kind};bookBaseline=JSON.stringify(bookDraft);bookError='';renderPlaybooks();}
        catch(error){if(!disposed&&ticket===bookTicket){bookError=error.message;renderPlaybooks();}}
      });
      if(!bookDraft)return;
      for(const [id,key] of [['v2BookName','name'],['v2BookBody','body']])$(id).oninput=e=>{bookDraft[key]=e.target.value;$('v2BookStatus').textContent=bookDirty()?'Unsaved draft':'';$('v2BookReview').replaceChildren();};
      $('v2BookSave').onclick=()=>submitBook('save');
      $('v2BookDelete').onclick=()=>{const id=bookDraft.id,revision=bookDraft.revision;$('v2BookReview').innerHTML='<p>Delete this saved playbook? Its operation audit and prior receipts remain, but it will leave the library.</p><button id="v2BookConfirmDelete">Confirm playbook delete</button>';$('v2BookConfirmDelete').onclick=()=>{if(bookDraft.id===id&&bookDraft.revision===revision)submitBook('delete');};};
      if($('v2BookRetry'))$('v2BookRetry').onclick=()=>submitBook();
    }
    async function submitBook(op){
      if(bookSaving||!bookDraft)return;
      if(!bookPending)bookPending={op,id:bookDraft.id,expectedRevision:bookDraft.revision,operationId:client.uuid(),...(op==='save'?{name:bookDraft.name,body:bookDraft.body,kind:bookDraft.kind}: {})};
      const operation=bookPending;bookSaving=true;bookError='';renderPlaybooks();
      try{
        const result=await client.mutatePlaybook(operation);
        if(disposed)return;
        bookPending=null;
        if(operation.op==='delete'){bookDraft=null;bookBaseline='';}
        else{const item=result.item;bookDraft={id:item.id,revision:item.revision,name:item.name,body:item.body,kind:item.kind};bookBaseline=JSON.stringify(bookDraft);}
        recoverBookOperation();
        await playbooks(bookAfter);
      }catch(error){if(!disposed){bookError=error.status===409?'Playbook changed. Your draft is retained; reload and compare before trying a new operation.':error.message;if(error.status===409||error.code==='invalid_playbook'||error.code==='missing_csrf')bookPending=null;}}
      finally{bookSaving=false;renderPlaybooks();}
    }
    async function playbooks(after=''){
      if(!client)return;const ticket=++bookTicket;
      try{const page=await client.playbooks(after);if(disposed||ticket!==bookTicket)return;bookPage=page;bookAfter=after;renderPlaybooks();}
      catch(error){if(!disposed&&ticket===bookTicket){bookError=error.message;renderPlaybooks();}}
    }
    async function standingOrders(after='',notice=''){
      if(!client)return;const ticket=++ordersTicket;
      pane.innerHTML=banner+'<p role="status">Loading standing orders and local targets…</p>';
      try{
        const [page,registry]=await Promise.all([client.standingOrders(after),client.guidanceRegistry()]);
        if(disposed||current!=='orders'||ticket!==ordersTicket)return;
        pane.innerHTML=banner+standingOrderMarkup(page,registry);
        $('v2RootStatus').textContent=notice;
        $('v2RootRefresh').onclick=()=>standingOrders(after);
        const confirmInline=(output,message,proceed)=>{
          const detail=document.createElement('pre'),yes=document.createElement('button'),no=document.createElement('button');
          detail.textContent=message;yes.textContent='Confirm action';no.textContent='Cancel action';output.replaceChildren(detail,yes,no);
          yes.onclick=()=>{yes.disabled=true;no.disabled=true;proceed();};
          no.onclick=()=>{output.textContent='Cancelled. No changes made.';};
          yes.focus();
        };
        const plantPanel=document.createElement('section');pane.append(plantPanel);
        const renderPlant=()=>{
          let recoveryError='';
          try{if(!plantPending)plantPending=client.pendingPlantOperations()[0]||null;}catch(error){recoveryError=error.message;}
          const draft=plantPending||plantDraft,locked=!!plantPending||plantBusy;
          plantPanel.innerHTML='<h3>Plant a standing order</h3><p>This writes a marked guidance block only to the targets you explicitly select. Commit and push are separate. Unsent drafts remain in this tab; submitted operations are saved locally for recovery.</p>'+
            `<label>Rule title <input id="v2PlantTitle" maxlength="100" value="${escape(draft.title)}" ${locked?'disabled':''}></label><label>Rule body <textarea id="v2PlantBody" rows="8" ${locked?'disabled':''}>${escape(draft.body)}</textarea></label><label>Topic <input id="v2PlantTopic" maxlength="40" value="${escape(draft.topic)}" ${locked?'disabled':''}></label><label>Review interval in days <input id="v2PlantDays" type="number" min="1" max="3650" value="${escape(draft.reviewEveryDays)}" ${locked?'disabled':''}></label><fieldset><legend>Files to change</legend>`+
            registry.targets.map(target=>`<label><input type="checkbox" data-plant-target="${escape(target.id)}" ${draft.targets.includes(target.id)?'checked':''} ${locked?'disabled':''}>${escape(target.label||target.name)} · ${escape(target.path)}</label>`).join('')+
            `</fieldset><button id="v2PlantSubmit" ${plantBusy||recoveryError?'disabled':''}>${plantPending?'Retry saved planting operation':'Review planting'}</button><div id="v2PlantStatus" role="status">${escape(recoveryError|| (plantPending?'A saved operation awaits acknowledgment. Its original content and target identities are preserved.':''))}</div>`;
          if(!locked){
            const edit=()=>{
              plantDraft={title:$('v2PlantTitle').value,body:$('v2PlantBody').value,topic:$('v2PlantTopic').value,reviewEveryDays:Number($('v2PlantDays').value),targets:[...plantPanel.querySelectorAll('[data-plant-target]:checked')].map(input=>input.dataset.plantTarget)};
              $('v2PlantStatus').textContent='Unsubmitted draft';
            };
            for(const input of plantPanel.querySelectorAll('input,textarea'))input.oninput=edit;
          }
          const submit=async(operation)=>{
            if(plantBusy)return;plantPending=operation;plantBusy=true;renderPlant();
            try{
              const result=await client.plantStandingOrder(operation);
              plantPending=null;plantBusy=false;plantDraft={title:'',body:'',topic:'general',reviewEveryDays:30,targets:[]};
              if(!disposed&&current==='orders'&&ticket===ordersTicket)await standingOrders(after,`Rule registered. ${result.results.map(row=>`${row.label||row.id||'Target'}: ${row.status}${row.error?' — '+row.error:''}`).join('; ')}. No commit or push was performed.`);
            }catch(error){if(plantPanel.isConnected){renderPlant();$('v2PlantStatus').textContent=error.message+' The saved operation is preserved; do not create a replacement to bypass an uncertain result.';}}
            finally{plantBusy=false;if(plantPanel.isConnected)$('v2PlantSubmit').disabled=!!recoveryError;}
          };
          $('v2PlantSubmit').onclick=()=>{
            if(plantPending){submit(plantPending);return;}
            const operation={op:'plant',operationId:client.uuid(),...plantDraft,targets:[...plantDraft.targets]};
            try{client._validatePlant(operation);}catch(error){$('v2PlantStatus').textContent=error.message;return;}
            const paths=operation.targets.map(id=>registry.targets.find(target=>target.id===id)?.path||id);
            confirmInline($('v2PlantStatus'),`Plant "${operation.title}"?\n\n${operation.body}\n\nFiles:\n${paths.join('\n')}\nNo commit or push will run.`,()=>submit(operation));
          };
        };
        renderPlant();
        const changeRoot=async(op,path,confirmed=false)=>{
          if(!path.trim()){$('v2RootStatus').textContent='Enter a full repository path.';return;}
          const question=op==='add-root'?`Authorize local guidance operations within this repository?\n${path}`:`Remove this added root from the registry? Existing guidance remains, and configuration or discovery may still grant access.\n${path}`;
          if(!confirmed){confirmInline($('v2RootStatus'),question,()=>changeRoot(op,path,true));return;}
          const output=$('v2RootStatus'),controls=[...pane.querySelectorAll('#v2RootAdd,#v2RootPath,[data-root-remove]')];
          controls.forEach(control=>control.disabled=true);output.textContent='Updating the local registry…';
          try{
            const result=await client.changeGuidanceRoot(op,path);
            if(disposed||current!=='orders'||ticket!==ordersTicket)return;
            await standingOrders(after,op==='add-root'?'Repository root added. No guidance file was changed.':`Registry updated. ${result.stranded} standing orders reference targets under the removed root. No guidance file was changed.`);
          }catch(error){if(output.isConnected)output.textContent=error.message+' Refresh the registry to check its current state before another change.';}
          // Keep mutations disabled after uncertain outcomes; a fresh registry read
          // is required instead of a blind repeat of an operation without receipts.
        };
        $('v2RootAdd').onclick=()=>changeRoot('add-root',$('v2RootPath').value);
        pane.querySelectorAll('[data-root-remove]').forEach(button=>button.onclick=()=>changeRoot('remove-root',button.dataset.rootRemove));
        $('v2OrdersFirst').onclick=()=>standingOrders();$('v2OrdersNext').onclick=()=>standingOrders(page.nextCursor);$('v2OrdersBooks').onclick=()=>{current='playbooks';renderPlaybooks();};
        pane.querySelectorAll('[data-order-check]').forEach(button=>button.onclick=async()=>{
          button.disabled=true;const output=button.nextElementSibling;output.textContent='Checking local files…';
          try{const result=await client.checkStandingOrder(button.dataset.orderCheck);if(disposed||current!=='orders'||ticket!==ordersTicket||!output.isConnected)return;output.textContent=result.statuses.map(s=>`${s.label||s.path}: ${s.status}; registry ${s.registryState||'unknown'}`).join('\n');}
          catch(error){if(output.isConnected)output.textContent=error.message;}
          finally{if(button.isConnected)button.disabled=false;}
        });
        pane.querySelectorAll('[data-order-review]').forEach(button=>{
          const rule=page.items.find(item=>item.id===button.dataset.orderReview),output=button.nextElementSibling;
          const retire=document.createElement('button'),retireOutput=document.createElement('div');
          retire.textContent=`Retire ${rule.title} from its files`;retire.disabled=!rule.stateHash;
          retireOutput.setAttribute('role','status');output.insertAdjacentElement('afterend',retire);retire.insertAdjacentElement('afterend',retireOutput);
          if(rule.topic==='model-tiering'&&rule.stateHash){
            const measurePanel=document.createElement('section'),previewButton=document.createElement('button'),measuredBody=document.createElement('pre'),measureStatus=document.createElement('p'),measureApply=document.createElement('button');
            previewButton.textContent=rule.pendingMeasurement?'Preview pending measurement recovery':'Preview updated measurements';
            measureApply.textContent='Apply this preview to registered files';measureApply.hidden=true;measureStatus.setAttribute('role','status');
            measurePanel.append(previewButton,measureStatus,measuredBody,measureApply);retireOutput.parentElement.append(measurePanel);
            const draft=new MeasurementDraft(client,rule,state=>{
              if(disposed||current!=='orders'||ticket!==ordersTicket||!measurePanel.isConnected){state.close();return;}
              previewButton.disabled=state.busy||state.finished;measureApply.disabled=state.busy||state.finished;measureApply.hidden=!state.preview;
              measuredBody.textContent=state.preview?.body||'';
              measureStatus.textContent=state.error||(state.busy?'Working…':state.result?(state.result.complete?'Applied to all registered targets. ':'Application remains pending. ')+state.result.results.map(r=>`${r.label||r.path||'Target'}: ${r.status}${r.error?' — '+r.error:''}`).join('; ')+'. Refresh and check files. No commit or push was performed.':state.preview?`Review this exact replacement before applying to these files:\n${(rule.targets||[]).map(t=>t.path).join('\n')}\nPolicy outside the measurement region is preserved; changed files are refused. No commit or push will run.`:'Preview does not change guidance files. At least 200 measured Claude child sources are required.');
            });
            previewButton.onclick=()=>draft.load();measureApply.onclick=()=>draft.apply();draft.emit();
          }
          const applyLabel=document.createElement('label'),applySelect=document.createElement('select'),applyButton=document.createElement('button'),applyOutput=document.createElement('div');
          applyLabel.textContent=`Target for ${rule.title} `;applyLabel.append(applySelect);
          for(const target of registry.targets){const option=document.createElement('option');option.value=target.id;option.textContent=`${target.label||target.name} · ${target.path}`;applySelect.append(option);}
          applyButton.textContent=`Apply ${rule.title} to selected target`;applyButton.disabled=!rule.stateHash||!!rule.pendingMeasurement||!registry.targets.length;
          applyOutput.setAttribute('role','status');retireOutput.parentElement.append(applyLabel,applyButton,applyOutput);
          applySelect.onchange=()=>{applyOutput.textContent='Selection changed; review this target before applying.';};
          applyButton.onclick=()=>{
            const target=registry.targets.find(item=>item.id===applySelect.value);if(!target)return;
            confirmInline(applyOutput,`Apply the displayed rule "${rule.title}" to:\n${target.path}\n\n${rule.body}\nExisting changed blocks are refused. No commit or push will run.`,async()=>{
              applyButton.disabled=true;applySelect.disabled=true;applyOutput.textContent='Applying to the selected file…';
              try{
                const result=await client.applyStandingOrder(rule.id,rule.stateHash,target.id);
                if(!applyOutput.isConnected)return;
                const row=result.results[0];applyOutput.textContent=`${row.label||target.path}: ${row.status}${row.error?' — '+row.error:''}. Refresh the registry and check the target before another action. No commit or push was performed.`;
              }catch(error){if(applyOutput.isConnected)applyOutput.textContent=error.message+' Application may have partially completed. Refresh and inspect the target before another attempt.';}
            });
          };
          for(const target of rule.targets||[]){
            const checkGit=document.createElement('button'),gitOutput=document.createElement('pre');
            const commitGit=document.createElement('button'),pushGit=document.createElement('button'),gitActionOutput=document.createElement('div');
            let gitState=null,gitBusy=false;
            commitGit.textContent=`Commit file locally: ${target.label||target.path}`;pushGit.textContent=`Push repository: ${target.label||target.path}`;
            commitGit.disabled=pushGit.disabled=true;gitActionOutput.setAttribute('role','status');
            checkGit.textContent=`Check repository for ${target.label||target.path}`;gitOutput.setAttribute('role','status');
            retireOutput.parentElement.append(checkGit,gitOutput,commitGit,pushGit,gitActionOutput);
            const gitAction=async(op,confirmed=false)=>{
              if(gitBusy||!gitState)return;
              if(!confirmed){
                const scope=`Path: ${target.path}\nRepository: ${gitState.root||'unknown'}\nLast observed branch: ${gitState.branch||'unknown'}`;
                const warning=op==='git-commit'?`Commit this entire guidance file locally?\n${scope}\nThis includes other edits in this file, not only the standing-order block. Other files are excluded. Nothing is pushed. Inspect the file diff before confirming.`:`Run the repository's configured Git push?\n${scope}\nThis can publish other commits and configured refs, not only this standing order. It does not create a commit. Inspect outgoing history and remote configuration before confirming.`;
                confirmInline(gitActionOutput,warning,()=>gitAction(op,true));return;
              }
              gitBusy=true;checkGit.disabled=commitGit.disabled=pushGit.disabled=true;gitActionOutput.textContent='Running explicit Git action…';
              try{const result=await client.standingOrderGitAction(op,rule.id,target.path,rule.stateHash);if(gitActionOutput.isConnected)gitActionOutput.textContent=(result.uncertain?'Outcome uncertain; partial changes may have occurred. ':result.done?'Completed. ':'Not completed. ')+result.note+' Check repository status again before another action.';}
              catch(error){if(gitActionOutput.isConnected)gitActionOutput.textContent=error.message;}
              finally{gitBusy=false;gitState=null;if(checkGit.isConnected)checkGit.disabled=false;}
            };
            commitGit.onclick=()=>gitAction('git-commit');pushGit.onclick=()=>gitAction('git-push');
            checkGit.onclick=async()=>{
              if(gitBusy)return;
              gitState=null;commitGit.disabled=pushGit.disabled=true;gitActionOutput.replaceChildren();
              checkGit.disabled=true;gitOutput.textContent='Reading local repository status…';
              try{
                const state=await client.standingOrderGitStatus(rule.id,target.path);
                if(!gitOutput.isConnected)return;
                gitState=state;
                commitGit.disabled=!(rule.stateHash&&state.isRepo===true&&state.branch&&state.fileDirty===true&&state.ignored===false);
                pushGit.disabled=!(rule.stateHash&&state.isRepo===true&&state.branch&&Number.isSafeInteger(state.ahead)&&state.ahead>0);
                const known=value=>value===null||value===undefined?'unknown':String(value);
                gitOutput.textContent=`Path: ${state.path}\nRepository: ${known(state.isRepo)}\nBranch: ${state.branch===''?'detached':known(state.branch)}\nFile has changes: ${known(state.fileDirty)}\nIgnored: ${known(state.ignored)}\nRule marker in HEAD: ${known(state.committed)}\nCommits ahead of upstream: ${known(state.ahead)}`+(state.error?'\nError: '+state.error:'')+Object.entries(state.probeErrors||{}).map(([key,error])=>`\n${key}: ${error}`).join('')+'\nNo commit or push was performed.';
              }catch(error){if(gitOutput.isConnected)gitOutput.textContent=error.message;}
              finally{if(checkGit.isConnected)checkGit.disabled=false;}
            };
          }
          const retireRule=async(confirmed=false)=>{
            const paths=(rule.targets||[]).map(target=>target.path).join('\n');
            if(!confirmed){confirmInline(retireOutput,`Remove this standing-order block from these files?\n${paths||'(No registered targets; remove the rule record only.)'}\nUnrelated text is preserved. Changed blocks are refused. This does not commit or push Git changes.`,()=>retireRule(true));return;}
            retire.disabled=true;button.disabled=true;retireOutput.textContent='Retiring registered blocks…';
            try{
              const result=await client.retireStandingOrder(rule.id,rule.stateHash);
              if(!retireOutput.isConnected)return;
              retireOutput.textContent=(result.removed?'Rule retired.':'Some targets remain unresolved; the rule is retained.')+'\n'+result.results.map(row=>`${row.label||'Target'}: ${row.status}${row.error?' — '+row.error:''}`).join('\n')+(result.needsCommit.length?'\nUncommitted repository changes: '+result.needsCommit.join(', '):'')+'\nNo commit or push was performed. Refresh the registry to inspect current state.';
            }catch(error){if(retireOutput.isConnected)retireOutput.textContent=error.message+' Retirement may have partially completed. Refresh the registry and check its files before another attempt.';}
          };
          retire.onclick=()=>retireRule();
          let operation=null;
          const recordReview=async(confirmed=false)=>{
            if(!operation){
              if(!confirmed){confirmInline(output,`Mark the displayed version of "${rule.title}" reviewed? This records your review; it does not change guidance files or certify their state.`,()=>recordReview(true));return;}
              operation={id:rule.id,expectedHash:rule.stateHash,operationId:client.uuid()};
            }
            button.disabled=true;output.textContent='Recording this review…';
            try{
              await client.reviewStandingOrder(operation);
              if(disposed||current!=='orders'||ticket!==ordersTicket)return;
              await standingOrders(after,'Review recorded. No guidance file was changed.');
            }catch(error){
              if(!output.isConnected)return;
              output.textContent=error.message;
              if(error.status>=400&&error.status<500&&![408,429].includes(error.status)){
                output.textContent+=' Refresh and inspect the current rule before reviewing again.';
              }else{
                button.textContent='Retry this exact review';button.disabled=false;
              }
            }
          };
          button.onclick=()=>recordReview();
        });
      }catch(error){if(!disposed&&current==='orders'&&ticket===ordersTicket){pane.innerHTML=banner+'<p role="alert">'+escape(error.message)+'</p><button id="v2OrdersRetry">Retry standing orders</button>';$('v2OrdersRetry').onclick=()=>standingOrders(after);}}
    }
    const banner = '';
    function renderRhythm(state){
      if(disposed||current!=='rhythm')return;
      if(!state){pane.innerHTML=banner+'<p>Loading rhythm…</p>';return;}
      pane.innerHTML=banner+rhythmMarkup(state);
      if(home){const header=pane.querySelector('.fleet-head');header.insertAdjacentHTML('beforeend',home.markup('rhythm'));home.wire(pane,'rhythm',()=>renderRhythm(state));}
      $('v2RhythmMetric').onchange=e=>{state.metric=e.target.value;renderRhythm(state);};
      $('v2RhythmRefresh').onclick=()=>state.load();
    }
    function renderCalendar(state){
      if(disposed||current!=='calendar')return;
      pane.innerHTML=banner+calendarMarkup(state);
      if(home){const header=pane.querySelector('.fleet-head');header.insertAdjacentHTML('beforeend',home.markup('calendar'));home.wire(pane,'calendar',()=>renderCalendar(state));}
      $('v2CalendarMetric').onchange=e=>{state.metric=e.target.value;renderCalendar(state);};
      $('v2CalendarRefresh').onclick=()=>state.load();
      pane.querySelectorAll('[data-calendar-day]').forEach(el=>el.onclick=()=>state.selectDay(el.dataset.calendarDay));
      if(state.dayModel){
        $('v2CalendarDayClose').onclick=()=>state.closeDay();$('v2CalendarDayRefresh').onclick=()=>state.dayModel.load(true);
        $('v2CalendarDayBack').onclick=()=>state.dayModel.back();$('v2CalendarDayNext').onclick=()=>state.dayModel.next();
        pane.querySelectorAll('[data-calendar-session]').forEach(el=>el.onclick=()=>detail(el.dataset.calendarSession));
      }
    }
    function renderUsage(state){
      if(disposed||current!=='usage')return;
      const t=state.totals;
      let budgetValue='';try{budgetValue=localStorage.getItem('mc-daily-budget')||'';}catch{}
      pane.innerHTML=banner+`<h2>Usage history</h2><p>Groups use recorded usage observations across complete indexed history. A session can appear in multiple groups; group session counts must not be added together.</p>
        <section><label for="v2DailyBudget">Daily estimate alert ($, 0 disables)</label><input id="v2DailyBudget" type="number" min="0" step="0.01" value="${escape(budgetValue)}"><button id="v2SaveBudget">Save budget and check</button><p id="v2BudgetStatus" role="status">${escape(budgetAlerts?.message||'Budget checks are unavailable.')}</p><p>Uses directly attributed estimates for usage recorded today in your local timezone, including archives. Unknown costs are not zero. Alerts run while this dashboard is visible, at most once per day. These estimates are not invoices and may include duplicated source history.</p></section>
        <div class="fleet-head"><label for="v2UsageGroup">Group by</label><select id="v2UsageGroup">${[['day','Day (UTC)'],['week','Week starting Monday (UTC)'],['month','Month (UTC)'],['year','Year (UTC)'],['model','Model'],['provider','Provider'],['machine','Machine'],['project','Project'],['agentKind','Agent kind']].map(([v,l])=>`<option value="${v}" ${state.dimension===v?'selected':''}>${l}</option>`).join('')}</select>
        <label for="v2UsageProvider">Provider</label><select id="v2UsageProvider"><option value="">All providers</option>${['claude','codex','otel'].map(v=>`<option ${state.query.provider===v?'selected':''}>${v}</option>`).join('')}</select>
        <label for="v2UsageArchive">Archive visibility</label><select id="v2UsageArchive">${[['all','All history'],['false','Active'],['true','Archived']].map(([v,l])=>`<option value="${v}" ${(state.query.archived==null?'all':String(state.query.archived))===v?'selected':''}>${l}</option>`).join('')}</select><button id="v2UsageRefresh">Refresh usage</button></div>
        <p>Session ledger totals: ${escape(t?.sessions??'unknown')} sessions · ${escape(t?.recordedTokens??'unknown')} tokens · recorded estimates ${t?.costEstimate==null?'unavailable':'~$'+escape(Number(t.costEstimate).toFixed(3))}</p>
        <p>${escape(coverageSummary(t))}</p><p>Imported summary totals may lack detailed usage observations. Group costs are not proportionally inferred from session estimates.</p>
        <p role="status">${escape(state.error||(state.loading?'Loading usage…':''))}</p>
        <label for="v2UsageMetric">Chart metric</label><select id="v2UsageMetric">${[['tokens','Recorded tokens (including cache)'],['sessions','Sessions in group'],['observations','Usage observations']].map(([v,l])=>`<option value="${v}" ${(state.metric||'tokens')===v?'selected':''}>${l}</option>`).join('')}</select>
        ${usageChart(state.rows,state.metric||'tokens')}
        <div class="table-wrap"><table class="ftable"><thead><tr><th>Group</th><th>Sessions in group</th><th>Observations</th><th>Input</th><th>Cache reads</th><th>Cache writes</th><th>Output</th><th>Unknown-model tokens</th><th>Attributable estimate</th><th>Priced / recorded tokens</th><th>Unpriced or unmeasured tokens</th></tr></thead><tbody>${state.rows.map(r=>`<tr><td>${escape(r.label||r.key)}</td><td>${escape(r.sessions)}</td><td>${escape(r.observations)}</td><td>${escape(r.tokensIn)}</td><td>${escape(r.tokensCache)}</td><td>${escape(r.tokensCacheWrite)}</td><td>${escape(r.tokensOut)}</td><td>${escape(r.unknownModelTokens??'unknown')}</td>${groupPriceCells(r)}</tr>`).join('')}</tbody></table></div>
        <p>Estimates cover directly priced observations only. Missing attribution and aggregate-only historical prices are not distributed across groups. Unpriced usage is not free. These are selected historical estimates, not current-rate comparisons or invoices.</p>
        <p>${state.rows.length} groups on this page.</p><button id="v2UsageBack" ${state.loading||!state.previous.length?'disabled':''}>Previous groups</button><button id="v2UsageNext" ${state.loading||!state.nextCursor?'disabled':''}>Next groups</button>`;
      $('v2UsageGroup').onchange=e=>state.filter({},e.target.value);$('v2UsageProvider').onchange=e=>state.filter({provider:e.target.value});
      $('v2UsageArchive').onchange=e=>state.filter({archived:e.target.value==='all'?undefined:e.target.value==='true'});
      $('v2UsageRefresh').onclick=()=>state.load();$('v2UsageBack').onclick=()=>state.back();$('v2UsageNext').onclick=()=>state.next();
      $('v2UsageMetric').onchange=e=>state.setMetric(e.target.value);
      $('v2SaveBudget').onclick=async()=>{
        const value=Number($('v2DailyBudget').value),output=$('v2BudgetStatus');
        if(!Number.isFinite(value)||value<0){output.textContent='Enter a finite, nonnegative dollar amount.';return;}
        try{localStorage.setItem('mc-daily-budget',String(value));await budgetAlerts?.poll(true);output.textContent=budgetAlerts?.message||'Budget saved; checks unavailable.';}
        catch{output.textContent='Budget could not be saved in this browser.';}
      };
    }
    function showMachineLabelHistory(id) {
      const dialog=document.createElement('dialog');
      dialog.innerHTML=`<h3>Machine rename history</h3><p>${escape(id)}</p><p>Recorded display-label changes, oldest first. Clearing a label restores the reported hostname; no source transcript is edited.</p><p role="status"></p><div data-history></div><button data-first>First page</button><button data-next disabled>Next page</button><button data-close>Close history</button>`;
      document.body.appendChild(dialog);
      const status=dialog.querySelector('[role="status"]'),items=dialog.querySelector('[data-history]'),first=dialog.querySelector('[data-first]'),next=dialog.querySelector('[data-next]');
      let closed=false,busy=false,cursor=0;
      dialog.addEventListener('close',()=>{closed=true;dialog.remove();});dialog.querySelector('[data-close]').onclick=()=>dialog.close();
      async function load(after){
        if(closed||busy)return;busy=true;first.disabled=true;next.disabled=true;status.textContent='Loading rename history…';
        try{
          const page=await client.machineLabelHistory(id,{after,limit:50});if(closed)return;
          items.innerHTML=page.items.map(item=>`<article><h4>Revision ${escape(item.revision)}</h4><p>${escape(item.at)}</p><p>Before: ${item.before.displayName?escape(item.before.displayName):'No override'}</p><p>After: ${item.after.displayName?escape(item.after.displayName):'No override'}</p><p>Operation: ${escape(item.operationId)}</p></article>`).join('')||'<p>No more recorded label changes.</p>';
          cursor=page.next||0;status.textContent='Showing '+page.items.length+' changes on this page.';
        }catch(error){if(!closed)status.textContent=error.message+' Use First page or Next page to retry; previously loaded entries remain visible.';}
        finally{busy=false;if(!closed){first.disabled=false;next.disabled=!cursor;}}
      }
      first.onclick=()=>load(0);next.onclick=()=>load(cursor);dialog.showModal();load(0);
    }
    function editMachineLabel(id) {
      const dialog=document.createElement('dialog');
      dialog.innerHTML=`<form><h3>Rename machine</h3><p>${escape(id)}</p><p>Display label only. Blank uses the reported hostname. Machine identity and chats are unchanged.</p><label>Display label<input name="label" maxlength="1024" disabled></label><p role="status">Loading label…</p><button type="submit" disabled>Save label</button><button type="button" data-close>Close editor</button></form>`;
      document.body.appendChild(dialog);
      const input=dialog.querySelector('input'),submit=dialog.querySelector('[type="submit"]'),status=dialog.querySelector('[role="status"]');
      let closed=false,busy=false,label=null,operation=null;
      dialog.addEventListener('close',()=>{closed=true;dialog.remove();});dialog.querySelector('[data-close]').onclick=()=>dialog.close();
      dialog.querySelector('form').onsubmit=async e=>{
        e.preventDefault();if(busy||!label)return;busy=true;input.disabled=true;submit.disabled=true;
        status.textContent='Rename pending. You may close this editor; the saved operation can be retried after reconnecting.';
        try{
          operation=operation||{machineId:id,displayName:input.value.trim(),revision:label.revision,operationId:crypto.randomUUID(),recoveryEpoch:client.recoveryEpoch};
          await client.mutateMachineLabel(operation);
          if(!closed)dialog.close();fleet();
        }catch(error){
          if(!closed){status.textContent=error.message+' Close and reopen the editor to reload after a conflict.';submit.textContent='Retry saved rename';submit.disabled=false;}
        }finally{busy=false;}
      };
      dialog.showModal();
      (async()=>{
        try{
          const pending=client.pendingMachineLabel(id);
          const currentLabel=pending?{machineId:id,revision:pending.revision,displayName:pending.displayName}:await client.machineLabel(id);
          if(closed)return;label=currentLabel;operation=pending;input.value=pending?.displayName??label.displayName;input.disabled=!!pending;submit.disabled=false;
          submit.textContent=pending?'Retry saved rename':'Save label';status.textContent=pending?'An earlier rename is unresolved. Retry its original operation before making another edit.':'';
        }catch(error){if(!closed)status.textContent=error.message;}
      })();
    }
    function showMachineSources(id) {
      const dialog=document.createElement('dialog');
      dialog.className='v2-source-dialog';
      dialog.innerHTML=`<h3>Retained transcripts</h3><p>${escape(id)}</p><p>Includes older generations and sources without indexed sessions. Organization archive flags do not hide raw evidence. Captured bytes may be incomplete.</p><form data-filter><fieldset data-filter-fields><label>Recorded path or indexed title contains <input data-path maxlength="1024"></label><button>Filter retained transcripts</button></fieldset></form><p>Literal substring matching, case-insensitive for ASCII. Uses indexed session titles or owner names when available; does not search transcript contents.</p><p role="status"></p><div data-sources></div><button data-first>Refresh first page</button><button data-next disabled>Next page</button><button data-close>Close transcripts</button>`;
      document.body.append(dialog);
      let closed=false,busy=false,next='',appliedQuery='';
      const lifetime=new AbortController();
      const status=dialog.querySelector('[role="status"]'),list=dialog.querySelector('[data-sources]'),first=dialog.querySelector('[data-first]'),more=dialog.querySelector('[data-next]');
      dialog.addEventListener('close',()=>{closed=true;lifetime.abort();dialog.remove();},{once:true});
      dialog.querySelector('[data-close]').onclick=()=>dialog.close();
      async function load(cursor='',q=appliedQuery) {
        if(closed||busy)return;busy=true;first.disabled=true;more.disabled=true;dialog.querySelector('[data-filter-fields]').disabled=true;status.textContent='Loading retained source page…';
        try {
          const page=await client.machineSources(id,{cursor,limit:100,q,signal:lifetime.signal});
          if(closed)return;
          next=page.nextCursor||'';appliedQuery=q;
          list.innerHTML=page.items.map((item,i)=>`<article class="ev"><h4>${escape(item.title||item.source.path||item.source.sourceId)}</h4><p>${escape(item.source.path)}</p><p>${escape(item.source.provider)} · generation ${escape(item.source.generation)} · ${item.activeGeneration?'Current generation':'Retained older generation'}</p><p>${escape(item.durableOffset)} captured bytes · ${escape(item.indexedOffset)} indexed bytes · ${escape(item.source.size)} last reported source bytes</p><a download href="${escape(client.rawSourceURL(item.source.sourceId,item.source.generation))}">Download captured source</a>${item.sessionId?` <button data-source-session="${i}">Open indexed session</button>`:'<p>No indexed session is available for this generation.</p>'}</article>`).join('')||'<p>No retained sources on this page.</p>';
          list.querySelectorAll('[data-source-session]').forEach(button=>button.onclick=()=>{const item=page.items[Number(button.dataset.sourceSession)];dialog.close();detail(item.sessionId);});
          status.textContent=`${page.items.length} sources on this page${appliedQuery?' · path/title contains: '+appliedQuery:''}. This is a live catalog; refresh from the first page to see earlier inserts.`;
        }catch(error){if(!closed)status.textContent=error.message;}
        finally{busy=false;if(!closed){first.disabled=false;more.disabled=!next;dialog.querySelector('[data-filter-fields]').disabled=false;}}
      }
      dialog.querySelector('[data-filter]').onsubmit=e=>{e.preventDefault();return load('',dialog.querySelector('[data-path]').value);};
      first.onclick=()=>load();more.onclick=()=>load(next);dialog.showModal();load();
    }
    function fleet(cursor=fleetCursor) {
      if(disposed||!client||!['fleet','machines'].includes(current))return Promise.resolve();
      if(fleetFlight)return fleetFlight;
      const view=current;
      clearTimeout(fleetTimer);fleetCursor=cursor;
      if(document.hidden){fleetTimer=setTimeout(()=>fleet(),10000);return Promise.resolve();}
      const ticket=detailTicket;
      fleetFlight=(async()=>{
        try{
          const [machines,history,health]=await Promise.all([client.machines(),client.machineCatalog({limit:100,cursor,archived:undefined}),client.health()]);
          if(disposed||current!==view||ticket!==detailTicket)return;
          const cards=fleetCards(machines,history.items);
          const restorePosition=preserveViewPosition(pane);
          pane.innerHTML=`<div class="fleet-head"><h2>${view==='machines'?'Machines':'Fleet'}</h2><span class="v2-state connected">${cards.filter(c=>c.connection==='connected').length} connected</span><span class="dim">${cards.length} machines</span></div><details class="v2-diagnostics" id="v2HubDiagnostics"><summary>Hub diagnostics · storage ${escape(health.storage?.state||'unknown')}</summary><p>Heartbeat connection status is independent of chat activity. Historical session counts include archives; they are not active-session or agent counts.</p>
            <p>Hub storage: ${escape(health.storage?.state||'unknown')} · hub indexing: ${escape(health.indexing?.state||'unknown')} · unindexed retained bytes (including older generations): ${escape(health.ledger?.indexingBacklogBytes??'unknown')}</p><p>Retained unindexed bytes are not necessarily active work waiting to be indexed. Superseded generations stay available as source evidence.</p>${storageObservationMarkup(health.storage)}
            ${ledgerObservationMarkup(health.ledgerObservation,health.ledgerError)}${checkpointMarkup(health.ledger?.checkpoint)}
            ${economicsSamplerMarkup(health.economicsHistory)}
            </details><div class="fleet-head"><button id="v2FleetRefresh">Refresh fleet</button></div><div class="fleet-grid">${cards.map(c=>`<article class="fcard fleet-card"><div class="v2-card-heading"><h3 title="${escape(c.name)}">${escape(c.name)}</h3><span class="v2-state ${['connected','delayed','disconnected'].includes(c.connection)?c.connection:'unknown'}">${escape(c.connection)}</span></div><div class="fstats"><span><b>${c.sessions==null?'—':Number(c.sessions).toLocaleString()}</b> sessions</span><span><b>${c.backlog==null?'—':Number(c.backlog).toLocaleString()}</b> bytes queued</span><span>Capture <b>${escape(c.collectionState||'unknown')}</b></span></div><div class="fdate">Last seen ${c.lastSeen?escape(new Date(c.lastSeen).toLocaleString()):'not recorded'}</div><details class="v2-machine-details" id="v2MachineDetails-${escape(c.id)}"><summary>Machine details</summary><p>${escape(c.id)}</p>${machineVersionMarkup(c.version,health.version)}${machineNetworkMarkup(c.network)}
              <p>Connection: ${escape(c.connection)} · last heartbeat: ${escape(c.lastSeen||'not recorded')}${c.age==null?'':` (${escape(c.age)} seconds ago)`}</p>
              <p>Capture: ${escape(c.collectionState||'unknown')} · uploads: ${escape(c.uploadState||'unknown')} · upload backlog: ${escape(c.backlog??'unknown')} bytes</p>${c.uploadRetryAt?`<p>Upload retry scheduled: ${escape(c.uploadRetryAt)} (last reported by collector)</p>`:''}<p>Captured: ${escape(c.captured??'unknown')} bytes · uploaded: ${escape(c.uploaded??'unknown')} bytes · last upload: ${escape(c.lastUpload||'not recorded')}</p>
              <p>${c.sourceOnly?'Retained raw sources; no indexed sessions yet.':c.sessions==null?'Historical count is outside this catalog page.':escape(c.sessions)+' historical sessions'} · chat activity: not inferred from heartbeat</p>
              <p ${c.historyGaps>0?'role="alert"':''}>Recorded history gaps: ${c.historyGaps==null?'not reported by this collector':escape(c.historyGaps)}${c.historyGaps>0?'. The collector recorded missing or changed source evidence; this count alone does not establish whether those bytes were later recovered.':''}</p>
              <p>${escape(coverageSummary(c.accounting))} · recorded estimates: ${c.costEstimate==null?'unavailable':'~$'+escape(Number(c.costEstimate).toFixed(3))}</p><p>Latest recorded activity: ${escape(c.lastActivity||'unknown')}</p>
              </details>${c.error?`<p role="alert">${escape(c.error)}</p>`:''}<div class="v2-machine-actions"><button data-machine-sessions="${escape(c.id)}">View sessions</button></div></article>`).join('')||'<p>No collector heartbeats or historical machines recorded.</p>'}</div>
            <p>Showing live collector records and one bounded historical machine page.</p><button id="v2FleetFirst" ${cursor?'':'disabled'}>First history page</button><button id="v2FleetNext" ${history.nextCursor?'':'disabled'}>Next history page</button>`;
          $('v2FleetRefresh').onclick=()=>fleet();$('v2FleetFirst').onclick=()=>fleet('');$('v2FleetNext').onclick=()=>fleet(history.nextCursor);
          pane.querySelectorAll('[data-machine-sessions]').forEach(el=>{el.onclick=()=>{clearTimeout(fleetTimer);current='table';detailTicket++;model.filter({machineId:el.dataset.machineSessions,provider:'',q:'',project:'',archived:undefined});};const rename=document.createElement('button');rename.textContent='Rename machine';rename.onclick=()=>editMachineLabel(el.dataset.machineSessions);el.after(rename);const history=document.createElement('button');history.textContent='Rename history';history.onclick=()=>showMachineLabelHistory(el.dataset.machineSessions);rename.after(history);const sources=document.createElement('button');sources.textContent='Browse retained transcripts';sources.onclick=()=>showMachineSources(el.dataset.machineSessions);history.after(sources);});
          restorePosition();
        }catch(error){if(current===view&&ticket===detailTicket)pane.innerHTML=banner+`<h2>${view==='machines'?'Machines':'Fleet'} unavailable</h2><p role="alert">${escape(error.message)}</p><p>Connection status is unknown while the hub cannot be read. Retrying…</p>`;}
        finally{fleetFlight=null;if(!disposed&&['fleet','machines'].includes(current))fleetTimer=setTimeout(()=>fleet(),current===view?10000:0);}
      })();return fleetFlight;
    }
    function render(state) {
      if (current !== 'table') return;
      if(state.loading&&state.rows.length&&!state.pending.length&&!state.error)return;
      const restorePosition=preserveViewPosition(pane);
      const focused = document.activeElement?.id, position = document.activeElement?.selectionStart;
      const draftValue = document.activeElement?.tagName === 'INPUT' ? document.activeElement.value : null;
      const q = state.query;
      pane.innerHTML = banner + `<div class="fleet-head"><h2>${state.totals ? escape(state.totals.sessions) : '…'} matching sessions · ${state.rows.length} on this page</h2>
        <input id="v2Search" placeholder="Search sessions" value="${escape(q.q || '')}">
        <select id="v2Provider" aria-label="Provider"><option value="">All providers</option>${['claude','codex','otel'].map(p=>`<option ${q.provider===p?'selected':''}>${p}</option>`).join('')}</select>
        <select id="v2Archive" aria-label="Archive visibility">${[['false','Active'],['true','Archived'],['all','All']].map(([v,l])=>`<option value="${v}" ${(q.archived==null?'all':String(q.archived))===v?'selected':''}>${l}</option>`).join('')}</select>
        ${projectFilterMarkup(q)}<input id="v2Machine" placeholder="Machine ID (exact)" value="${escape(q.machineId || '')}">
        <button id="v2Refresh">Refresh</button></div>
        ${q.from||q.to?`<p>Activity range: ${escape(q.from||'beginning')} to ${escape(q.to||'present')} (end excluded) <button id="v2ClearDateRange">Clear date range</button></p>`:''}
        <details id="v2PricingDetails" class="v2-pricing-summary"><summary>${escape(briefPricing(state.totals))}</summary><p>${escape(coverageSummary(state.totals))}</p><button id="v2CompareContributions">Compare recorded and reconciled tokens</button></details>
        <div role="status" id="v2Status">${escape([state.error || (state.loading && !state.rows.length ? 'Loading…' : ''), state.totalsError ? 'Totals unavailable: '+state.totalsError : ''].filter(Boolean).join(' · '))}${state.pending.length ? ' · '+state.pending.length+' organization changes pending' : ''}</div>
        <div class="table-wrap"><table class="ftable"><thead><tr><th data-sort="title">Session</th><th>Kind</th><th>Machine</th><th>Project</th><th>Events</th><th data-sort="tokens">Tokens</th><th data-sort="cost">Cost estimate</th><th data-sort="lastActivity">When</th><th>Organization</th></tr></thead><tbody>${state.rows.map(row => {
          const m = row.metadata;
          const tokens = row.tokensIn + row.tokensCache + row.tokensCacheWrite + row.tokensOut;
          const price=pricingDisplay(row);
          return `<tr data-id="${escape(row.id)}"><td class="tsess"><button data-detail="${escape(row.id)}" title="${escape(m.name || row.title || row.id)}">${m.pinned?'★ ':''}${escape(m.name || row.title || row.id)}</button>${m.note?`<span class="mini-badge note" title="${escape(m.note)}">✎</span>`:''}${(m.tags||[]).slice(0,5).map(tag=>`<span class="mini-badge">${escape(tag)}</span>`).join('')}${m.tags?.length>5?`<span class="mini-badge">+${m.tags.length-5} tags</span>`:''}</td><td><span class="kind-badge">${escape(row.provider)}</span></td><td>${escape(row.machineName||row.machineId)}${row.machineName&&row.machineName!==row.machineId?`<br><small>Machine ID: ${escape(row.machineId)}</small>`:''}</td><td title="${escape(m.projectOverride || m.project ? m.project : row.project)}">${escape(m.projectOverride || m.project ? m.project : row.project)}</td><td class="num">${escape(Number(row.eventCount).toLocaleString())}</td><td class="num">${escape(Number(tokens).toLocaleString())}</td><td class="num" title="${escape(price.detail)}">${escape(price.cost)}<small class="v2-coverage">${escape(price.coverage)}</small></td><td title="${escape(row.lastActivity)}">${escape(new Date(row.lastActivity).toLocaleDateString())}</td><td><button data-archive="${escape(row.id)}">${m.archived?'Unarchive':'Archive'}</button><button data-pin="${escape(row.id)}" aria-pressed="${!!m.pinned}">${m.pinned?'Unpin':'Pin'}</button><button data-edit="${escape(row.id)}">Edit details</button>${m.pending?' pending':''}</td></tr>`;
        }).join('')}</tbody></table></div><div class="fleet-head"><button id="v2Back" ${!state.hasPrevious||state.loading?'disabled':''}>Previous</button><button id="v2Next" ${!state.hasNext||state.loading?'disabled':''}>Next</button></div>`;
      $('v2Search').oninput = e => { clearTimeout(searchTimer); const q = e.target.value; searchTimer = setTimeout(()=>model.filter({q}),200); };
      $('v2CompareContributions').onclick=()=>inspectContributionTotals({...q});
      $('v2Provider').onchange = e => model.filter({ provider: e.target.value });
      $('v2Archive').onchange = e => model.filter({ archived: e.target.value==='all'?undefined:e.target.value==='true' });
      $('v2Project').oninput = e => { clearTimeout(searchTimer); const project=e.target.value; searchTimer=setTimeout(()=>model.projectFilter(project),200); };
      $('v2UnassignedProject').onchange = e => { clearTimeout(searchTimer); model.projectFilter('',e.target.checked); };
      $('v2Machine').onchange = e => model.filter({ machineId: e.target.value });
      $('v2Refresh').onclick = () => model.load(true);
      if($('v2ClearDateRange'))$('v2ClearDateRange').onclick=()=>model.filter({from:undefined,to:undefined});
      $('v2Back').onclick = () => model.back(); $('v2Next').onclick = () => model.next();
      pane.querySelectorAll('[data-sort]').forEach(el=>el.onclick=()=>model.filter({sort:el.dataset.sort,direction:q.sort===el.dataset.sort&&q.direction==='desc'?'asc':'desc'}));
      pane.querySelectorAll('[data-archive]').forEach(el=>el.onclick=()=>model.archive(el.dataset.archive));
      pane.querySelectorAll('[data-pin]').forEach(el=>el.onclick=()=>{try{model.organize(el.dataset.pin,{pinned:!client.organization(el.dataset.pin).pinned});}catch(error){model.error=error.message;model.emit();}});
      pane.querySelectorAll('[data-edit]').forEach(el=>el.onclick=()=>editOrganization(state.rows.find(row=>row.id===el.dataset.edit)));
      pane.querySelectorAll('[data-id]').forEach(el=>{
        const button=document.createElement('button');button.textContent='Pricing';button.onclick=()=>editPricing(el.dataset.id);
        el.lastElementChild.append(button);
      });
      pane.querySelectorAll('[data-detail]').forEach(el=>el.onclick=()=>detail(el.dataset.detail));
      if (focused && $(focused)) { if (draftValue != null) $(focused).value = draftValue; $(focused).focus(); if (typeof position==='number' && $(focused).setSelectionRange) $(focused).setSelectionRange(position,position); }
      restorePosition();
    }
    async function inspectContributionTotals(query) {
      if(document.getElementById('v2ContributionTotalsDialog'))return;
      const dialog=document.createElement('dialog'),controller=new AbortController();
      dialog.id='v2ContributionTotalsDialog';dialog.setAttribute('aria-label','Contribution accounting comparison');
      const content=document.createElement('section'),close=document.createElement('button');
      content.textContent='Loading filtered contribution totals…';close.textContent='Close';close.onclick=()=>dialog.close();
      dialog.append(content,close);document.body.appendChild(dialog);
      let closed=false;
      dialog.addEventListener('close',()=>{closed=true;controller.abort();dialog.remove();},{once:true});dialog.showModal();
      try{
        const totals=await client.contributionTotals({...query,signal:controller.signal});
        if(disposed||closed||!dialog.isConnected)return;
        content.innerHTML=contributionTotalsMarkup(totals);
      }catch(error){
        if(disposed||closed||!dialog.isConnected)return;
        content.textContent='Contribution comparison unavailable: '+error.message;
      }
    }

    function editOrganization(row) {
      if (!row || document.getElementById('v2Editor')) return;
      const draft = new OrganizationDraft({...row,metadata:client.organization(row.id)});
      const dialog = document.createElement('dialog'); dialog.id='v2Editor';dialog.setAttribute('aria-labelledby','v2EditorTitle');
      dialog.innerHTML=`<form><h2 id="v2EditorTitle">Session details</h2><p>Only changed fields are saved. Background refreshes will not replace this draft.</p>
        <label for="v2EditName">Name</label><input id="v2EditName" value="${escape(draft.values.name)}">
        <label for="v2EditNote">Note</label><textarea id="v2EditNote" rows="6">${escape(draft.values.note)}</textarea>
        <label for="v2EditTags">Tags (one per line)</label><textarea id="v2EditTags" rows="3">${escape(draft.values.tags)}</textarea>
        <label for="v2EditProject">Project assignment</label><input id="v2EditProject" value="${escape(draft.values.project)}"><button type="button" id="v2Unassign">Unassign project</button>
        <p id="v2EditError" role="alert"></p><div><button type="submit">Save changes</button><button type="button" id="v2EditCancel">Cancel</button></div></form>`;
      document.body.append(dialog);
      const read = () => { for(const key of ['Name','Note','Tags','Project']) draft.values[key.toLowerCase()]=dialog.querySelector('#v2Edit'+key).value; };
      dialog.querySelector('#v2Unassign').onclick=()=>{draft.unassign();dialog.querySelector('#v2EditProject').value='';};
      dialog.querySelector('#v2EditCancel').onclick=()=>dialog.close();
      dialog.addEventListener('close',()=>dialog.remove(),{once:true});
      dialog.querySelector('form').onsubmit=e=>{
        e.preventDefault();read();const patch=draft.patch();
        if(!Object.keys(patch).length){dialog.close();return;}
        try { model.organize(row.id,patch);dialog.close(); }
        catch(error){dialog.querySelector('#v2EditError').textContent=error.message;}
      };
      dialog.showModal();
    }
    function editPricing(id) {
      if(document.getElementById('v2Pricing'))return;
      const dialog=document.createElement('dialog');dialog.id='v2Pricing';dialog.setAttribute('aria-labelledby','v2PricingTitle');
      dialog.innerHTML=`<form><h2 id="v2PricingTitle">Session pricing</h2><p>Choose an existing rate catalog and billing context. This changes estimates, never recorded tokens.</p>
        <p id="v2PolicyStatus" role="status">Loading…</p><label for="v2Catalog">Rate catalog</label><select id="v2Catalog" required></select>
        <button type="button" id="v2CatalogFirst">First catalogs</button><button type="button" id="v2CatalogNext" disabled>Next catalogs</button>
        <label for="v2Billing">Billing context</label><select id="v2Billing" required></select>
        <details><summary>Selected catalog evidence</summary><pre id="v2RateEvidence"></pre></details>
        <p id="v2CatalogPurpose" role="status"></p>
        <details><summary>Default for new sessions on all machines</summary><p>Apply the selected catalog, billing context, and comparison timestamp to future sessions only. Existing sessions and disabled policies stay unchanged. These are fixed-date API estimates, not subscription charges; rates do not refresh automatically.</p><p id="v2PricingDefaultStatus" role="status">Not loaded.</p><button type="button" id="v2PricingDefaultRefresh">Refresh default</button><button type="button" id="v2PricingDefaultEnable" disabled>Use selection for future sessions</button><button type="button" id="v2PricingDefaultDisable">Disable default for future sessions</button></details>
        <p>One automatic pricing mode per session. Enabling either mode replaces its existing automatic policy, but preserves saved estimates.</p>
        <button type="button" id="v2PricingAutoCompare" disabled>Enable automatic API-rate comparisons</button><p>Uses the comparison timestamp below and saves updated comparisons as this session is indexed. Rates stay fixed to the selected catalog and date; this is not your subscription bill.</p>
        <details><summary>Saved estimate history</summary><p>Historical estimates and hypothetical comparisons are retained separately. Pages use stable snapshot-ID order, not date order. New snapshots may require refreshing the first page. A historical entry is not necessarily the currently selected estimate.</p><button type="button" id="v2PricingHistoryFirst">Load or refresh saved estimates</button><button type="button" id="v2PricingHistoryNext" disabled>Next saved estimates</button><pre id="v2PricingHistory" role="status"></pre></details>
        <details><summary>Compare at a chosen rate date</summary><p>This saves a hypothetical comparison, not a bill. It does not replace historical pricing. Choose the billing context explicitly; session totals cannot establish the per-request context length.</p><label for="v2ComparisonAt">Comparison timestamp with timezone</label><input id="v2ComparisonAt" type="text" placeholder="2026-09-06T19:16:28Z"><button type="button" id="v2PricingCompare" disabled>Save rate comparison</button><pre id="v2ComparisonResult" role="status"></pre></details>
        <details><summary>Import rate catalog</summary><p>Paste catalog JSON (at most 64 KiB). Use a new catalog ID for changed rates or classifications. Every rate needs model, context, effectiveFrom, source, and explicit Input, CacheRead, CacheWrite, Output prices per million tokens. Optional tier: flagship, premium, mid, cheap, or unknown. Omitted tiers stay unknown. Import alone does not enable pricing.</p><label for="v2CatalogJSON">Catalog JSON</label><textarea id="v2CatalogJSON" rows="7" spellcheck="false"></textarea><button type="button" id="v2CatalogImport">Import catalog</button><p id="v2CatalogImportStatus" role="status"></p></details>
        <p id="v2PricingError" role="alert"></p><button type="submit" id="v2PricingSave" disabled>Enable automatic pricing</button>
        <button type="button" id="v2PricingDisable">Disable automatic pricing</button><button type="button" id="v2PricingRefresh">Refresh status</button><button type="button" id="v2PricingClose">Close</button></form>`;
      document.body.append(dialog);dialog.showModal();
      const find=id=>dialog.querySelector('#'+id);
      let closed=false,busy=false,ticket=0,listTicket=0,statusTicket=0,comparisonTicket=0,next='',policy=null,catalogReady=false,comparisonOnly=false;
      function pricingButtons(){if(closed)return;find('v2PricingSave').disabled=busy||!catalogReady||comparisonOnly;find('v2PricingCompare').disabled=busy||!catalogReady;find('v2PricingAutoCompare').disabled=busy||!catalogReady;find('v2PricingDisable').disabled=busy;find('v2PricingDefaultEnable').disabled=busy||!catalogReady;find('v2PricingDefaultDisable').disabled=busy;}
      const error=e=>{if(!closed)find('v2PricingError').textContent=e.message;};
      let defaultTicket=0;
      async function defaultStatus(){
        const mine=++defaultTicket;
        try{const p=await client.pricingDefault();if(closed||mine!==defaultTicket)return;find('v2PricingDefaultStatus').textContent=p?`Future sessions: ${p.catalogId}; ${p.context}; comparison date ${p.comparisonAt}. Existing sessions unchanged.`:'Default disabled. Existing policies and estimates are retained.';}
        catch(e){if(!closed&&mine===defaultTicket){find('v2PricingDefaultStatus').textContent='Default status unavailable.';error(e);}}
      }
      async function defaultMutate(disable){
        if(busy||(!disable&&!catalogReady))return;
        const selection=disable?null:{catalogId:find('v2Catalog').value,context:find('v2Billing').value,comparisonAt:find('v2ComparisonAt').value.trim()};
        if(selection&&(!selection.context||!/(Z|[+-]\d\d:\d\d)$/.test(selection.comparisonAt)||!Number.isFinite(Date.parse(selection.comparisonAt)))){error(new Error('Choose a billing context and comparison timestamp with a timezone.'));return;}
        defaultTicket++;busy=true;pricingButtons();find('v2PricingError').textContent='';
        try{await client.setPricingDefault(selection);await defaultStatus();}
        catch(e){if(!closed){find('v2PricingDefaultStatus').textContent='Default change not confirmed. Refresh to check before retrying.';error(e);}}
        finally{busy=false;pricingButtons();}
      }
      find('v2PricingDefaultRefresh').onclick=()=>defaultStatus();
      find('v2PricingDefaultEnable').onclick=()=>defaultMutate(false);
      find('v2PricingDefaultDisable').onclick=()=>defaultMutate(true);
      let historyTicket=0,historyNext='';
      async function estimateHistory(after=''){
        const mine=++historyTicket;find('v2PricingHistoryNext').disabled=true;find('v2PricingHistory').textContent='Loading saved estimates…';
        try{
          const page=await client.pricingHistory(id,after);if(closed||mine!==historyTicket)return;
          historyNext=page.next;find('v2PricingHistoryNext').disabled=!historyNext;
          find('v2PricingHistory').textContent=page.rows.length?page.rows.map(row=>{
            const p=row.estimate;
            return (row.comparisonAt?'Hypothetical comparison at '+row.comparisonAt:'Historical estimate')+'\nCatalog: '+row.catalogId+'\nBilling context: '+row.context+'\n'+(p.pricedTokens?'Known estimated cost: $'+p.cost:'Unpriced — no covered tokens')+'\nCoverage: '+p.pricedTokens+'/'+p.recordedTokens+' tokens priced; '+p.unpricedTokens+' unpriced; '+p.unattributedTokens+' unattributed.\nSnapshot: '+row.id+'\nSource generation: '+row.generation+'; projection: '+(row.projectionRevision||'baseline')+'; indexed bytes: '+row.indexedOffset;
          }).join('\n\n'):'No saved estimates on this page.';
        }catch(e){if(!closed&&mine===historyTicket){historyNext='';find('v2PricingHistory').textContent='Saved estimates could not be loaded. Use Load or refresh to retry.';error(e);}}
      }
      find('v2PricingHistoryFirst').onclick=()=>estimateHistory();find('v2PricingHistoryNext').onclick=()=>historyNext&&estimateHistory(historyNext);
      async function selected(context='') {
        comparisonTicket++;
        const mine=++ticket;catalogReady=false;comparisonOnly=false;pricingButtons();find('v2Billing').innerHTML='';find('v2RateEvidence').textContent='';find('v2CatalogPurpose').textContent='';find('v2ComparisonResult').textContent='';
        const catalogId=find('v2Catalog').value;if(!catalogId)return;
        const catalog=await client.rateCatalog(catalogId);if(closed||mine!==ticket)return;
        const contexts=[...new Set(catalog.rates.map(rate=>rate.context))];
        find('v2Billing').innerHTML='<option value="">Choose billing context</option>'+contexts.map(c=>`<option value="${escape(c)}">${escape(c)}</option>`).join('');
        if(contexts.includes(context))find('v2Billing').value=context;
        catalogReady=true;comparisonOnly=catalog.comparisonOnly===true;
        find('v2CatalogPurpose').textContent=comparisonOnly?'Comparison-only catalog: historical pricing is unavailable. Manual or automatic comparisons require a timestamp within its documented observation window.':'Historical pricing requires evidence that these rates and this billing context applied to the recorded usage.';
        find('v2RateEvidence').textContent=JSON.stringify(catalog,null,2);pricingButtons();
      }
      async function catalogs(after='') {
        comparisonTicket++;find('v2ComparisonResult').textContent='';
        const mine=++listTicket;ticket++;catalogReady=false;pricingButtons();
        const page=await client.rateCatalogs(after);if(closed||mine!==listTicket)return;
        next=page.next;find('v2CatalogNext').disabled=!next;
        const ids=page.ids;find('v2Catalog').innerHTML='<option value="">Choose rate catalog</option>'+ids.map(c=>`<option value="${escape(c)}">${escape(c)}</option>`).join('');
        if(policy&&!ids.includes(policy.catalogId)){const option=new Option(policy.catalogId,policy.catalogId);find('v2Catalog').append(option);}
        if(policy)find('v2Catalog').value=policy.catalogId;
        if(!ids.length&&!policy)find('v2PricingError').textContent='No rate catalogs are installed. Import a catalog with verified source references below.';
        await selected(policy?.context);
      }
      async function status(){const mine=++statusTicket;const p=await client.pricingPolicy(id);if(closed||mine!==statusTicket)return;policy=p;find('v2PolicyStatus').textContent=policyStatus(p);if(p?.comparisonAt&&!find('v2ComparisonAt').value)find('v2ComparisonAt').value=p.comparisonAt;}
      find('v2PricingAutoCompare').onclick=async()=>{
        if(busy||!catalogReady)return;
        const comparisonAt=find('v2ComparisonAt').value.trim();
        if(!find('v2Billing').value||!/(Z|[+-]\d\d:\d\d)$/.test(comparisonAt)||!Number.isFinite(Date.parse(comparisonAt))){error(new Error('Choose a billing context and comparison timestamp with a timezone.'));return;}
        busy=true;pricingButtons();find('v2PricingError').textContent='';
        try{await client.setPricingPolicy(id,{catalogId:find('v2Catalog').value,context:find('v2Billing').value,comparisonAt});await status();}
        catch(e){error(e);}finally{busy=false;pricingButtons();}
      };
      async function mutate(disable){
        if(!disable&&(!catalogReady||comparisonOnly)){error(new Error('This catalog cannot enable historical automatic pricing.'));return;}
        if(busy)return;busy=true;find('v2PricingSave').disabled=true;find('v2PricingDisable').disabled=true;find('v2PricingError').textContent='';
        pricingButtons();
        try{await client.setPricingPolicy(id,disable?null:{catalogId:find('v2Catalog').value,context:find('v2Billing').value});await status();}
        catch(e){error(e);}finally{busy=false;pricingButtons();}
      }
      find('v2PricingCompare').onclick=async()=>{
        if(busy||!catalogReady)return;
        const mine=++comparisonTicket;busy=true;pricingButtons();find('v2PricingError').textContent='';find('v2ComparisonResult').textContent='Saving comparison…';
        try{
          const result=await client.comparePricing(id,{catalogId:find('v2Catalog').value,context:find('v2Billing').value,comparisonAt:find('v2ComparisonAt').value.trim()});
          if(closed||mine!==comparisonTicket)return;
          find('v2ComparisonResult').textContent='Saved hypothetical comparison; historical pricing unchanged.\n'+JSON.stringify(result,null,2);
        }catch(e){if(!closed&&mine===comparisonTicket){find('v2ComparisonResult').textContent='Comparison not confirmed. A connection failure does not prove that saving failed; retrying may return the saved snapshot.';error(e);}}
        finally{busy=false;pricingButtons();}
      };
      find('v2Catalog').onchange=()=>selected().catch(error);
      const comparisonChanged=()=>{comparisonTicket++;find('v2ComparisonResult').textContent='';};
      find('v2Billing').onchange=comparisonChanged;find('v2ComparisonAt').oninput=comparisonChanged;
      find('v2CatalogFirst').onclick=()=>catalogs().catch(error);find('v2CatalogNext').onclick=()=>catalogs(next).catch(error);
      find('v2PricingRefresh').onclick=()=>status().catch(error);find('v2PricingDisable').onclick=()=>mutate(true);
      find('v2CatalogImport').onclick=async()=>{
        find('v2CatalogImportStatus').textContent='';
        if(busy)return;busy=true;find('v2CatalogImport').disabled=true;find('v2PricingSave').disabled=true;find('v2PricingDisable').disabled=true;find('v2PricingError').textContent='';
        pricingButtons();
        try{
          const result=await client.importRateCatalog(find('v2CatalogJSON').value);
          if(closed)return;
          find('v2CatalogImportStatus').textContent='Catalog '+result.catalogId+' imported. Review its purpose and billing context before using it.';
          await catalogs();if(closed)return;
          if(![...find('v2Catalog').options].some(o=>o.value===result.catalogId))find('v2Catalog').append(new Option(result.catalogId,result.catalogId));
          find('v2Catalog').value=result.catalogId;await selected();
        }catch(e){error(e);}finally{busy=false;if(!closed)find('v2CatalogImport').disabled=false;pricingButtons();}
      };
      find('v2PricingClose').onclick=()=>dialog.close();dialog.querySelector('form').onsubmit=e=>{e.preventDefault();mutate(false);};
      dialog.addEventListener('close',()=>{closed=true;ticket++;dialog.remove();},{once:true});
      status().then(()=>catalogs()).catch(error);
    }
    function agentActivity(id,agent) {
      const dialog=document.createElement('dialog');
      dialog.setAttribute('aria-label','Agent activity');
      dialog.style.cssText='width:min(900px,90vw);max-height:85vh;overflow:auto';
      dialog.innerHTML=`<h2>Activity: ${escape(agent.name||agent.id||'Unattributed')}</h2><button data-close>Close agent activity</button><section data-tool-summary><button data-show-tools>Show full-history tool usage</button></section><div data-activity></div>`;
      document.body.appendChild(dialog);dialog.showModal();
      let closed=false,ticket=0;
      dialog.querySelector('[data-close]').onclick=()=>dialog.close();
      dialog.addEventListener('close',()=>{closed=true;ticket++;dialog.remove();},{once:true});
      const output=dialog.querySelector('[data-activity]');
      const lifecycle=document.createElement('section');lifecycle.innerHTML='<button data-lifecycle>Check retry and pending-tool evidence</button>';output.before(lifecycle);
      async function loadLifecycle(){
        lifecycle.textContent='Checking indexed tool lifecycle…';
        try{
          const value=await client.agentLifecycle(id,agent.id);if(closed)return;
          lifecycle.innerHTML=`<p>Retry marker: ${value.retrying===null?'unknown':value.retrying?'last recorded tool result failed':'not set'}. Current stall heuristic: ${value.stalled===null?'unknown':value.stalled?'triggered':'not triggered'}. ${value.uncertainCalls} ambiguous or unmatched identities.</p><p>A missing result is not proof that a tool is still running. The stall heuristic requires a call older than two minutes and a source modified within ten minutes. This does not measure satellite connectivity.</p>${value.pending?'<button data-pending-record>Open latest unresolved call</button>':''}<button data-lifecycle>Refresh lifecycle evidence</button>`;
          if(value.pending)lifecycle.querySelector('[data-pending-record]').onclick=()=>{dialog.close();detail(id,0,value.snapshot,value.pending.sequence);};
        }catch(error){if(closed)return;lifecycle.innerHTML=`<p role="alert">${escape(error.message)}</p><button data-lifecycle>Retry lifecycle check</button>`;}
        lifecycle.querySelector('[data-lifecycle]').onclick=loadLifecycle;
      }
      lifecycle.querySelector('[data-lifecycle]').onclick=loadLifecycle;
      let toolTicket=0;
      async function loadTools(cursor='') {
        const mine=++toolTicket,summary=dialog.querySelector('[data-tool-summary]');
        summary.textContent='Loading full-history tool counts…';
        try {
          const page=await client.agentTools(id,agent.id,{cursor,limit:5});
          if(closed||mine!==toolTicket)return;
          summary.innerHTML=agentToolsMarkup(page)+`<button data-tools-first>Refresh first tool page</button><button data-tools-next ${page.nextCursor?'':'disabled'}>Next tool page</button>`;
          summary.querySelector('[data-tools-first]').onclick=()=>loadTools();
          summary.querySelector('[data-tools-next]').onclick=()=>loadTools(page.nextCursor);
        }catch(error){
          if(closed||mine!==toolTicket)return;
          summary.innerHTML=`<p role="alert">${escape(error.message)}</p><button data-tools-retry>Retry full-history tool usage</button>`;
          summary.querySelector('[data-tools-retry]').onclick=()=>loadTools();
        }
      }
      dialog.querySelector('[data-show-tools]').onclick=()=>loadTools();
      async function load(after=0,snapshot='',previous=[]) {
        const mine=++ticket;output.textContent='Loading agent activity…';
        try {
          const page=await client.events(id,{agentId:agent.id,after,snapshot,limit:100});
          if(closed||mine!==ticket)return;
          output.innerHTML=agentActivityMarkup(page.events)+`<button data-first ${after?'':'disabled'}>First activity page</button><button data-previous ${previous.length?'':'disabled'}>Previous activity page</button><button data-next ${page.nextSequence?'':'disabled'}>Next activity page</button>`;
          output.querySelector('[data-first]').onclick=()=>load();
          output.querySelector('[data-previous]').onclick=()=>load(previous.at(-1),page.snapshot,previous.slice(0,-1));
          output.querySelector('[data-next]').onclick=()=>load(page.nextSequence,page.snapshot,[...previous,after].slice(-1000));
          output.querySelectorAll('[data-agent-record]').forEach(button=>button.onclick=()=>{
            const event=page.events.find(e=>e.sequence===Number(button.dataset.agentRecord));
            if(!event)return;dialog.close();detail(id,0,event.historySnapshot,event.sequence);
          });
        } catch(error) {
          if(closed||mine!==ticket)return;
          output.innerHTML=`<p role="alert">${escape(error.message)}</p><button data-retry>Reload first activity page</button>`;
          output.querySelector('[data-retry]').onclick=()=>load();
        }
      }
      load();
    }

    async function board(id=selectedSession,cursor='',previous=[]) {
      stopStory();
      const ticket=++detailTicket;current='board';
      if(!id){pane.innerHTML=banner+'<p>Open a session from the table, then choose Board.</p>';return;}
      selectingSession(id);
      pane.innerHTML=banner+'<p>Loading agent board…</p>';
      try {
        const [row,page,stats]=await Promise.all([client.session(id),client.agents(id,{limit:100,cursor}),client.stats(id)]);
        if(disposed||current!=='board'||ticket!==detailTicket)return;
        readySessionExport(row,id);
        pane.innerHTML=banner+`<h2>${escape(row.metadata.name||row.title||id)}</h2>${boardMarkup(page,stats)}<button id="v2BoardFirst" ${cursor?'':'disabled'}>First agent page</button><button id="v2BoardPrevious" ${previous.length?'':'disabled'}>Previous agent page</button><button id="v2BoardNext" ${page.nextCursor?'':'disabled'}>Next agent page</button><button id="v2BoardHistory">Open session history</button><section id="v2BoardRelationships"><button id="v2BoardLoadRelationships">Show recorded relationships</button></section>`;
        $('v2BoardFirst').onclick=()=>board(id);
        $('v2BoardPrevious').onclick=()=>board(id,previous.at(-1),previous.slice(0,-1));
        $('v2BoardNext').onclick=()=>board(id,page.nextCursor,[...previous,cursor].slice(-1000));
        $('v2BoardHistory').onclick=()=>detail(id);
        pane.querySelectorAll('.v2-agent-board article').forEach((card,index)=>{
          const agent=page.agents[index],button=document.createElement('button');
          button.textContent='View agent activity';card.appendChild(button);
          button.onclick=()=>agentActivity(id,agent);
        });
        const refresh=document.createElement('button');refresh.textContent='Refresh Board';refresh.onclick=()=>board(id,cursor,previous);pane.insertBefore(refresh,$('v2BoardHistory'));
        pane.querySelectorAll('[data-rename-agent]').forEach(button=>button.onclick=()=>{
          const agent=page.agents.find(a=>a.id===button.dataset.renameAgent),output=button.nextElementSibling;
          if(!agent)return;button.disabled=true;
          output.innerHTML=`<form><label>Agent display name <input name="agentName" maxlength="1024" value="${escape(agent.name||'')}"></label><p>Leave blank to use the recorded identity. This does not edit the transcript or other sessions.</p><button type="submit">Save agent name</button><button type="button">Cancel rename</button><p role="status"></p></form>`;
          const form=output.querySelector('form'),input=form.querySelector('input'),status=form.querySelector('[role="status"]');
          form.querySelector('[type="button"]').onclick=()=>{output.textContent='';button.disabled=false;};
          form.onsubmit=event=>{
            event.preventDefault();
            let completion;
            try{completion=client.organize(id,{agentName:{id:agent.id,name:input.value}});}catch(error){status.textContent=error.message;return;}
            form.querySelectorAll('button,input').forEach(el=>el.disabled=true);status.textContent='Name change pending. Intent is retained through reconnects.';
            completion.then(()=>{if(!disposed&&current==='board'&&ticket===detailTicket)board(id,cursor,previous);},error=>{if(status.isConnected)status.textContent=error.message+' Refresh the Board to inspect the current name.';});
          };
        });
        let relationRead=0;
        const relationships=async(after='')=>{
          const mine=++relationRead;const output=$('v2BoardRelationships');output.textContent='Loading recorded relationships…';
          try {
            const page=await client.lineage(id,{limit:100,cursor:after});
            if(disposed||current!=='board'||ticket!==detailTicket||mine!==relationRead)return;
            output.innerHTML=lineageMarkup(page);
            output.querySelectorAll('[data-related-session]').forEach(button=>button.onclick=()=>{selectedSession=button.dataset.relatedSession;board(selectedSession);});
            $('v2LineageFirst').onclick=()=>relationships();$('v2LineageNext').onclick=()=>relationships(page.nextCursor);
          }catch(error){
            if(disposed||current!=='board'||ticket!==detailTicket||mine!==relationRead)return;
            output.innerHTML=`<p role="alert">${escape(error.message)}</p><button id="v2BoardRelationRetry">Retry relationships</button>`;$('v2BoardRelationRetry').onclick=()=>relationships(after);
          }
        };
        $('v2BoardLoadRelationships').onclick=()=>relationships();
      }catch(error){
        if(disposed||current!=='board'||ticket!==detailTicket)return;
        selectingSession(id);
        pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button id="v2BoardRetry">Reload first agent page</button>`;$('v2BoardRetry').onclick=()=>board(id);
      }
    }
    async function timeline(id=selectedSession, after=0, snapshot='', anchor=null, view='timeline', direction='before', autoplay=null) {
      if(autoplay===null)stopStory();
      const ticket=++detailTicket;current=view;
      if(!id){pane.innerHTML=banner+'<p>Open a session from the table, then choose a session view.</p>';return;}
      selectingSession(id);
      pane.innerHTML=banner+'<p>Loading session view…</p>';
      try {
        const [row,page]=await Promise.all([client.session(id),historyPage(client,id,{after,snapshot,anchor,direction})]);
        if(disposed||current!==view||ticket!==detailTicket)return;
        readySessionExport(row,id);
        pane.innerHTML=banner+`<h2>Session ${view} · ${escape(row.metadata.name||row.title||id)}</h2>${view==='story'||view==='lanes'?`<label>Records revealed on this page <input id="v2StoryScrub" type="range" min="0" max="${page.events.length}" value="${page.events.length}" step="1"></label><button id="v2StoryBack">Previous record</button><button id="v2StoryForward">Next record</button><button id="v2StoryPlay">Play records</button><button id="v2StoryPause" disabled>Pause playback</button><label>Playback speed <select id="v2StorySpeed">${[1,4,16].map(speed=>`<option value="${speed}" ${storySpeed===speed?'selected':''}>${speed}×</option>`).join('')}</select></label><p>Playback reveals 1, 4 or 16 records every 180 ms, not at original event timing. It pauses when this tab is hidden.</p><p id="v2StoryPosition" role="status"></p><form id="v2StoryJump"><label>Jump to recorded sequence <input id="v2StorySequence" inputmode="numeric" required></label><button type="submit">Jump</button><p id="v2StoryJumpError" role="alert"></p></form><div id="v2StoryBody"></div>`:timelineMarkup(page.events)}<button id="v2TimelineFirst">First ${view} page</button><button id="v2TimelinePrevious" ${page.hasPrevious&&page.events.length?'':'disabled'}>Previous ${view} page</button><button id="v2TimelineNext" ${page.nextSequence?'':'disabled'}>Next ${view} page</button><button id="v2TimelineHistory">Open session history</button>`;
        $('v2TimelineFirst').onclick=()=>timeline(id,0,'',null,view);
        $('v2TimelinePrevious').onclick=()=>timeline(id,0,page.snapshot,page.events[0].sequence,view);
        $('v2TimelineNext').onclick=()=>timeline(id,page.nextSequence,page.snapshot,null,view);
        $('v2TimelineHistory').onclick=()=>detail(id,after,page.snapshot,anchor,anchor==null?'around':direction);
        pane.querySelectorAll('[data-timeline-sequence]').forEach(button=>button.onclick=()=>detail(id,0,page.snapshot,Number(button.dataset.timelineSequence)));
        if(view==='story'||view==='lanes'){
          const anchorIndex=direction==='around'&&anchor!=null?page.events.findIndex(event=>event.sequence===anchor):-1;
          let revealed=autoplay!==null?0:anchorIndex>=0?anchorIndex+1:page.events.length;
          const active=()=>!disposed&&current===view&&ticket===detailTicket;
          const reveal=value=>{
            if(!active()||!Number.isInteger(value)||value<0||value>page.events.length)return;
            revealed=value;$('v2StoryScrub').value=String(value);
            $('v2StoryBack').disabled=value===0;$('v2StoryForward').disabled=value===page.events.length;
            $('v2StoryPosition').textContent=`${value} / ${page.events.length} records on this page${value?' · through sequence '+page.events[value-1].sequence:''}. Use page navigation for earlier or later history.`;
            $('v2StoryBody').innerHTML=(view==='story'?storyMarkup:lanesMarkup)(page.events.slice(0,value));
            $('v2StoryBody').querySelectorAll('[data-timeline-sequence]').forEach(button=>button.onclick=()=>{if(active())return detail(id,0,page.snapshot,Number(button.dataset.timelineSequence));});
          };
          $('v2StoryScrub').oninput=()=>{if(!active())return;stopStory();reveal(Number($('v2StoryScrub').value));};
          $('v2StoryBack').onclick=()=>{if(!active())return;stopStory();reveal(revealed-1);};
          $('v2StoryForward').onclick=()=>{if(!active())return;stopStory();reveal(revealed+1);};
          const play=epoch=>{
            if(!active()||document.hidden||epoch!==storyPlayEpoch)return;
            storyPlaying=true;
            $('v2StoryPlay').disabled=true;$('v2StoryPause').disabled=false;
            const tick=()=>{
              if(!active()||document.hidden||epoch!==storyPlayEpoch){if(epoch===storyPlayEpoch)stopStory();return;}
              storyTimer=null;
              if(revealed<page.events.length){reveal(Math.min(page.events.length,revealed+storySpeed));storyTimer=setTimeout(tick,180);}
              else if(page.nextSequence&&page.events.length){return timeline(id,page.nextSequence,page.snapshot,null,view,'before',epoch);}
              else{stopStory();$('v2StoryPosition').textContent+=' Playback reached the end of this indexed snapshot.';}
            };
            storyTimer=setTimeout(tick,180);
          };
          $('v2StoryPlay').onclick=()=>{if(!active())return;stopStory();if(revealed===page.events.length)reveal(0);play(storyPlayEpoch);};
          $('v2StoryPause').onclick=()=>{if(active())stopStory();};
          $('v2StorySpeed').onchange=()=>{if(!active())return;const speed=Number($('v2StorySpeed').value);if([1,4,16].includes(speed))storySpeed=speed;else $('v2StorySpeed').value=String(storySpeed);};
          $('v2StoryJump').onsubmit=event=>{
            event.preventDefault();if(!active())return;
            const raw=$('v2StorySequence').value.trim(),sequence=Number(raw);
            if(!/^[1-9][0-9]*$/.test(raw)||!Number.isSafeInteger(sequence)){$('v2StoryJumpError').textContent='Enter a positive, safe integer record sequence.';return;}
            return timeline(id,0,page.snapshot,sequence,view,'around');
          };
          reveal(revealed);
          if(autoplay!==null)play(autoplay);
        }
      } catch(error) {
        if(disposed||current!==view||ticket!==detailTicket)return;
        stopStory();
        selectingSession(id);
        pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button id="v2TimelineRetry">Reload first ${view} page</button>`;
        $('v2TimelineRetry').onclick=()=>timeline(id,0,'',null,view);
      }
    }
    async function costflow(id=selectedSession,cursor='',previous=[],metric='cost',snapshot='') {
      stopStory();clearTimeout(fleetTimer);const ticket=++detailTicket;current='costflow';
      if(!id){pane.innerHTML=banner+'<p>Open a session first to view cost flow.</p>';return;}
      selectingSession(id);pane.innerHTML=banner+'<p>Loading cost flow…</p>';
      try{
        const page=await client.costFlow(id,{cursor,limit:100,snapshot});const row=page.session;
        if(disposed||current!=='costflow'||ticket!==detailTicket)return;
        readySessionExport(row,id);
        pane.innerHTML=banner+`<h2>Session cost flow · ${escape(row.metadata.name||row.title||id)}</h2><label>Flow weight <select id="v2FlowMetric"><option value="cost" ${metric==='cost'?'selected':''}>Known estimated cost</option><option value="output" ${metric==='output'?'selected':''}>Recorded output tokens</option></select></label>${costFlowMarkup(page.agents,metric,row,metric==='cost'?page.topCost:page.topOutput)}<button id="v2FlowFirst">Refresh first identity page</button><button id="v2FlowPrevious" ${previous.length?'':'disabled'}>Previous identity page</button><button id="v2FlowNext" ${page.nextCursor?'':'disabled'}>Next identity page</button><p>Previous retains the latest 20 page positions. All identities remain accessible through forward pagination.</p><button id="v2FlowHistory">Open session history</button>`;
        $('v2FlowMetric').onchange=e=>costflow(id,cursor,previous,e.target.value,page.snapshot);
        $('v2FlowFirst').onclick=()=>costflow(id,'',[],metric);
        $('v2FlowPrevious').onclick=()=>costflow(id,previous.at(-1),previous.slice(0,-1),metric,page.snapshot);
        $('v2FlowNext').onclick=()=>costflow(id,page.nextCursor,[...previous,cursor].slice(-20),metric,page.snapshot);
        $('v2FlowHistory').onclick=()=>detail(id);
      }catch(error){
        if(disposed||current!=='costflow'||ticket!==detailTicket)return;
        selectingSession(id);pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button id="v2FlowRetry">Retry identity page</button>`;
        if(error.code==='history_changed')$('v2FlowRetry').textContent='History changed: reload first identity page';
        $('v2FlowRetry').onclick=()=>error.code==='history_changed'?costflow(id,'',[],metric):costflow(id,cursor,previous,metric,snapshot);
      }
    }
    function priorUndoEdits(id,undoSequence,snapshot) {
      const dialog=document.createElement('dialog');dialog.setAttribute('aria-label','Edits before undo attempt');
      dialog.innerHTML='<h2>Earlier edit attempts</h2><p>Recorded Edit, Write, MultiEdit and NotebookEdit calls in this session, before this undo record. These paths were not necessarily affected by Git. Relative paths are not resolved. Shell scripts, patch tools and other editing mechanisms are outside this view.</p><div id="v2PriorEdits"></div><button id="v2PriorFirst">Reload earlier edits</button><button id="v2PriorNext" disabled>Older edit page</button><button id="v2PriorClose">Close</button>';
      document.body.append(dialog);dialog.showModal();let ticket=0,closed=false,controller,next=0;
      const node=id=>dialog.querySelector('#'+id);
      dialog.addEventListener('close',()=>{closed=true;ticket++;controller?.abort();dialog.remove();});node('v2PriorClose').onclick=()=>dialog.close();
      async function load(before=0){
        controller?.abort();controller=new AbortController();const current=++ticket;
        node('v2PriorNext').disabled=true;node('v2PriorEdits').textContent='Loading earlier edit evidence…';
        try{
          const page=await client.priorUndoEdits(id,undoSequence,{snapshot,before,limit:25,signal:controller.signal});
          if(closed||current!==ticket)return;next=page.nextBefore||0;
          node('v2PriorEdits').innerHTML=page.state==='needs-rebuild'?'<p>Prior edit context needs rebuilding. An empty result does not establish that nothing was edited.</p>':`<p>${page.diagnostics} whole-session indexing diagnostics. Repeated paths are separate attempts, newest earlier call first.</p>`+(page.edits.map((e,i)=>`<article class="ev"><p>${escape(e.tool)} · ${escape(e.path)}</p><button data-prior-edit="${i}">Open original edit record ${e.sequence}</button></article>`).join('')||'<p>No recognized earlier edit calls on this page of available indexed history.</p>');
          node('v2PriorNext').disabled=!next;
          dialog.querySelectorAll('[data-prior-edit]').forEach(button=>button.onclick=()=>{const e=page.edits[Number(button.dataset.priorEdit)];dialog.close();detail(id,0,snapshot,e.sequence);});
        }catch(error){if(!closed&&current===ticket)node('v2PriorEdits').textContent=error.message;}
      }
      node('v2PriorFirst').onclick=()=>load();node('v2PriorNext').onclick=()=>load(next);load();
    }
    async function fleetUndoProjects(cursor='') {
      stopStory();const ticket=++detailTicket;current='graveyard';selectingSession('');
      const controller=undoController=new AbortController();pane.innerHTML=banner+'<p role="status">Loading project ranking…</p>';
      try {
        const page=await client.gitUndoProjects({cursor,limit:25,signal:controller.signal});
        if(disposed||current!=='graveyard'||ticket!==detailTicket)return;
        pane.innerHTML=banner+'<h2>Graveyard · projects by known undo attempts</h2><p>Exact recorded session folders across machines, including archived chats. These are recognized attempts, not successful operations or complete coverage. Unknown and unsupported history is excluded; folders are not verified command working directories.</p>'+page.projects.map((p,i)=>`<article class="ev"><h3>${escape(p.project||'Project not recorded')}</h3><p>${p.attempts} known attempts · ${p.sessions} sessions</p><button data-undo-group="${i}">Show project attempts</button></article>`).join('')+(page.projects.length?'':'<p>No recognized attempts in available indexed history.</p>')+`<button id="v2UndoGroupsFirst">Reload project ranking</button><button id="v2UndoGroupsNext" ${page.nextCursor?'':'disabled'}>Next project page</button><button id="v2UndoGroupsBack">Back to undo history</button><p>Rankings changing during paging require a reload. No history is removed.</p>`;
        $('v2UndoGroupsFirst').onclick=()=>fleetUndoProjects();$('v2UndoGroupsNext').onclick=()=>fleetUndoProjects(page.nextCursor);$('v2UndoGroupsBack').onclick=()=>fleetUndoHistory();
        pane.querySelectorAll('[data-undo-group]').forEach(button=>button.onclick=()=>fleetUndoHistory('',[],page.projects[Number(button.dataset.undoGroup)].project));
      }catch(error){
        if(disposed||current!=='graveyard'||ticket!==detailTicket)return;
        pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button id="v2UndoGroupsFirst">Reload project ranking</button><button id="v2UndoGroupsBack">Back to undo history</button>`;
        $('v2UndoGroupsFirst').onclick=()=>fleetUndoProjects();$('v2UndoGroupsBack').onclick=()=>fleetUndoHistory();
      }
    }
    let flowsController,flowsUpdate,flowsArchive=false;
    async function flowsWork(cursor='',previous=[],archived=flowsArchive){
      flowsArchive=archived;
      flowsController?.abort();stopStory();const ticket=++detailTicket;current='flows';selectingSession('');
      const controller=flowsController=new AbortController(),active=()=>!disposed&&current==='flows'&&ticket===detailTicket&&!controller.signal.aborted;
      pane.innerHTML=banner+'<p role="status">Grouping indexed fleet behavior…</p>';
      try{
        const page=await client.behaviorPatterns({cursor,limit:20,archived,signal:controller.signal});if(!active())return;let error='',rolesPage=null,rolesBusy=false,rolesError='',rolesCursor='',rolesPrevious=[];
        const render=()=>{
          if(!active())return;pane.innerHTML=banner+flowsMarkup(page,id=>client.organization(id),archived,error,flowRolesMarkup(rolesPage,rolesBusy,rolesError)+`<button data-flow-roles-first ${rolesBusy?'disabled':''}>Refresh roles</button><button data-flow-roles-previous ${rolesBusy||!rolesPrevious.length?'disabled':''}>Previous roles</button><button data-flow-roles-next ${rolesBusy||!rolesPage?.nextCursor?'disabled':''}>Next roles</button>`)+`<label>Archive visibility <select data-flow-visibility><option value="active" ${archived===false?'selected':''}>Active</option><option value="archived" ${archived===true?'selected':''}>Archived</option><option value="all" ${archived===null?'selected':''}>All</option></select></label><button data-flow-first>Refresh first page</button><button data-flow-previous ${previous.length?'':'disabled'}>Previous patterns</button><button data-flow-next ${page.nextCursor?'':'disabled'}>Next patterns</button>`;
          pane.querySelector('[data-flow-visibility]').onchange=e=>flowsWork('',[],e.target.value==='all'?null:e.target.value==='archived');pane.querySelector('[data-flow-first]').onclick=()=>flowsWork('',[],archived);pane.querySelector('[data-flow-previous]').onclick=()=>flowsWork(previous.at(-1),previous.slice(0,-1),archived);pane.querySelector('[data-flow-next]').onclick=()=>flowsWork(page.nextCursor,[...previous,cursor].slice(-20),archived);
          pane.querySelectorAll('[data-flow-session]').forEach(b=>b.onclick=()=>{controller.abort();detail(page.patterns[Number(b.dataset.flowSession)].exampleSessionId);});
          pane.querySelectorAll('[data-flow-archive]').forEach(b=>b.onclick=()=>{const id=page.patterns[Number(b.dataset.flowArchive)].exampleSessionId;try{const done=client.archive(id,true);render();done.then(()=>{if(active())flowsWork('',[],archived);},e=>{if(active()){error=e.message;render();}});}catch(e){error=e.message;render();}});
          pane.querySelector('[data-flow-roles-first]').onclick=()=>loadRoles();pane.querySelector('[data-flow-roles-previous]').onclick=()=>loadRoles(rolesPrevious.at(-1),rolesPrevious.slice(0,-1));pane.querySelector('[data-flow-roles-next]').onclick=()=>loadRoles(rolesPage.nextCursor,[...rolesPrevious,rolesCursor].slice(-20));
          pane.querySelectorAll('[data-flow-role-session]').forEach(b=>b.onclick=()=>{controller.abort();detail(rolesPage.roles[Number(b.dataset.flowRoleSession)].exampleSessionId);});
        };
        const loadRoles=async(next='',trail=[])=>{
          if(!active()||rolesBusy)return;rolesBusy=true;rolesError='';render();
          try{const nextPage=await client.behaviorRoles({cursor:next,limit:20,archived,signal:controller.signal});if(active()){rolesPage=nextPage;rolesCursor=next;rolesPrevious=trail;}}
          catch(e){if(active()){rolesError=e.message;rolesPage=null;}}finally{rolesBusy=false;if(active())render();}
        };
        flowsUpdate=update=>{if(!active()||!page.patterns.some(p=>p.exampleSessionId===update.sessionID))return;if(archived===false&&update.metadata.archived&&!update.metadata.pending){flowsWork('',[],archived);return;}render();};render();void loadRoles();
      }catch(e){if(active()){pane.innerHTML=banner+`<p role="alert">${escape(e.message)}</p><button data-flow-first>Retry from first page</button>`;pane.querySelector('[data-flow-first]').onclick=()=>flowsWork('',[],archived);}}
    }
    let secretsController;
    async function secretsWork(cursor='',previous=[]) {
      secretsController?.abort();stopStory();const ticket=++detailTicket;current='leaks';selectingSession('');
      const controller=secretsController=new AbortController(),active=()=>!disposed&&current==='leaks'&&ticket===detailTicket&&!controller.signal.aborted;
      pane.innerHTML=banner+'<p>Loading local machine binding…</p>';
      try {
        const identity=await client.localIdentity({signal:controller.signal});if(!active())return;
        if(identity.state!=='configured'){pane.innerHTML=banner+'<h2>Secrets</h2><p>Configure the desktop machineId to match its local collector. No files have been read.</p>';return;}
        const page=await client.unsavedCandidates(identity.machineId,{cursor,limit:20,signal:controller.signal});if(!active())return;
        let checks=[],busy=false,error='';
        const render=()=>{
          if(!active())return;pane.innerHTML=banner+secretsMarkup(page,checks,busy,error)+'<button data-secrets-first>Refresh first page</button><button data-secrets-previous '+(previous.length?'':'disabled')+'>Previous candidate page</button><button data-secrets-next '+(page.nextCursor?'':'disabled')+'>Next candidate page</button>';
          pane.querySelector('[data-secrets-scan]').onclick=scan;
          pane.querySelector('[data-secrets-first]').onclick=()=>secretsWork();pane.querySelector('[data-secrets-previous]').onclick=()=>secretsWork(previous.at(-1),previous.slice(0,-1));pane.querySelector('[data-secrets-next]').onclick=()=>secretsWork(page.nextCursor,[...previous,cursor].slice(-20));
          pane.querySelectorAll('[data-secrets-session]').forEach(b=>b.onclick=()=>{controller.abort();detail(page.files[Number(b.dataset.secretsSession)].sessionId);});
          pane.querySelectorAll('[data-secrets-copy]').forEach(b=>b.onclick=async()=>{const i=Number(b.dataset.secretsCopy);try{await navigator.clipboard.writeText(checks[i]?.path||page.files[i].path);if(active())b.textContent='Path copied';}catch(e){if(active()){error='Could not copy the path. Select the displayed path to copy it manually.';render();}}});
        };
        const scan=async()=>{
          if(!active()||busy||!page.files.length)return;busy=true;error='';checks=[];render();
          try {const result=await client.checkLocalSecrets(identity.machineId,page.files,{signal:controller.signal});if(active())checks=result.files;}
          catch(e){if(active())error=e.message;}finally{busy=false;if(active())render();}
        };
        render();await scan();
      }catch(e){if(active())pane.innerHTML=banner+`<p role="alert">${escape(e.message)}</p>`;}
    }
    let unsavedController;
    async function unsavedWork(cursor='',previous=[]) {
      unsavedController?.abort();stopStory();const ticket=++detailTicket;current='unsaved';selectingSession('');
      const controller=unsavedController=new AbortController(),active=()=>!disposed&&current==='unsaved'&&ticket===detailTicket&&!controller.signal.aborted;
      pane.innerHTML=banner+'<p role="status">Loading local machine binding…</p>';
      try {
        const identity=await client.localIdentity({signal:controller.signal});if(!active())return;
        if(identity.state!=='configured'){
          pane.innerHTML=banner+'<h2>Unsaved Work</h2><p role="alert">The desktop has no configured local machine identity. Set its machineId to the local collector’s stable machine ID before checking transcript paths. No local files have been inspected. A matching path from a satellite is not evidence about this PC.</p><button data-unsaved-first>Retry local binding</button>';
          pane.querySelector('[data-unsaved-first]').onclick=()=>unsavedWork();return;
        }
        const page=await client.unsavedCandidates(identity.machineId,{cursor,limit:20,signal:controller.signal});if(!active())return;
        const checks=page.files.map(()=>null);let busy=false,error='';
        const eligible=p=>(/^[A-Za-z]:[\\/]/.test(p)||/^\/(?!\/)/.test(p))&&!/[\x00\r\n]/.test(p)&&!p.slice(/^[A-Za-z]:/.test(p)?2:0).includes(':');
        const render=()=>{
          if(!active())return;
          pane.innerHTML=banner+unsavedMarkup(page,checks,busy,error)+`<p><button data-unsaved-first>Refresh first page</button><button data-unsaved-previous ${previous.length?'':'disabled'}>Previous candidate page</button><button data-unsaved-next ${page.nextCursor?'':'disabled'}>Next candidate page</button></p><p>Local checks do not save, stage, commit or push files. Previous retains the latest 20 page positions.</p>`;
          pane.querySelector('[data-unsaved-first]').onclick=()=>unsavedWork();pane.querySelector('[data-unsaved-previous]').onclick=()=>unsavedWork(previous.at(-1),previous.slice(0,-1));pane.querySelector('[data-unsaved-next]').onclick=()=>unsavedWork(page.nextCursor,[...previous,cursor].slice(-20));
          pane.querySelector('[data-unsaved-rescan]').onclick=()=>check(page.files.map((_,i)=>i));
          pane.querySelectorAll('[data-unsaved-check]').forEach(button=>button.onclick=()=>check([Number(button.dataset.unsavedCheck)]));
          pane.querySelectorAll('[data-unsaved-session]').forEach(button=>button.onclick=()=>{controller.abort();detail(page.files[Number(button.dataset.unsavedSession)].sessionId);});
        };
        const check=async indices=>{
          if(!active()||busy)return;error='';
          const key=f=>JSON.stringify([f.path,f.workingDirectory||'']);
          const selected=indices.filter(i=>{const f=page.files[i];if(!eligible(f.path)&&!eligible(f.workingDirectory||'')){checks[i]={state:'unsupported-path',problem:'No local lookup was attempted: this path has no supported recorded working directory.'};return false;}return true;});
          const paths=[...new Map(selected.map(i=>{const f=page.files[i];return [key(f),{path:f.path,workingDirectory:f.workingDirectory||''}];})).values()];
          if(!paths.length){render();return;}
          busy=true;selected.forEach(i=>checks[i]={state:'checking'});render();
          try {const result=await client.checkLocalUnsaved(identity.machineId,paths,{signal:controller.signal});if(!active())return;const byPath=new Map(result.files.map(f=>[JSON.stringify([f.requestedPath,f.requestedWorkingDirectory||'']),f]));selected.forEach(i=>checks[i]=byPath.get(key(page.files[i])));}
          catch(e){if(!active())return;error=e.message;selected.forEach(i=>checks[i]={state:'unknown',problem:e.message});}
          finally{busy=false;if(active())render();}
        };
        render();await check(page.files.map((_,i)=>i));
      }catch(error){if(!active())return;pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button data-unsaved-first>Retry from first page</button>`;pane.querySelector('[data-unsaved-first]').onclick=()=>unsavedWork();}
    }
    let troubleSort='sessions',troubleDirection='desc';
    async function troubleFiles(cursor='',previous=[]){
      stopStory();const ticket=++detailTicket;current='trouble';selectingSession('');pane.innerHTML=banner+'<p>Loading indexed file evidence…</p>';
      try{
        const page=await client.troubleFiles({cursor,limit:20,sort:troubleSort,direction:troubleDirection});if(disposed||current!=='trouble'||ticket!==detailTicket)return;
        pane.innerHTML=banner+`<h2>Trouble Files</h2><p>${page.indexedSessions} of ${page.totalSessions} sessions have a current, diagnostic-free file-edit index. Includes archived sessions. Scope: recorded Edit, Write, MultiEdit and NotebookEdit attempts. Shell writes and filesystem aliases are not inferred; exact paths are separated by machine and recorded project.</p><p>Rates use only sessions touching that file, with a minimum of five and no unresolved outcome uncertainty. Errors, retry markers and current stall heuristics count as bad outcomes—not proof the file caused them. Each session counts once per file.</p><div class="table-wrap"><table class="ftable"><thead><tr>${[['path','File'],['sessions','Sessions'],['badSessions','Went badly'],['unknownSessions','Unknown'],['rate','Rate'],['lastTouched','Last touched']].map(([key,label])=>`<th aria-sort="${troubleSort===key?(troubleDirection==='asc'?'ascending':'descending'):'none'}"><button data-trouble-sort="${key}">${label}${troubleSort===key?(troubleDirection==='asc'?' ▲':' ▼'):''}</button></th>`).join('')}</tr></thead><tbody>${page.files.map((f,i)=>`<tr><td><button data-trouble-file="${i}">${escape(f.path)}</button><div class="dim">${escape(f.machineId)} · ${escape(f.project)}</div></td><td>${f.sessions}</td><td>${f.badSessions}</td><td>${f.unknownSessions}</td><td>${f.rate===null?'Insufficient evidence':f.rate.toFixed(1)+'%'}</td><td>${escape(f.lastTouched)}</td></tr>`).join('')}</tbody></table></div>${page.files.length?'':'<p>No matching indexed file attempts. This does not establish that no files were touched.</p>'}<button data-trouble-first>Refresh first page</button><button data-trouble-back ${previous.length?'':'disabled'}>Previous files</button><button data-trouble-next ${page.nextCursor?'':'disabled'}>Next files</button>`;
        pane.querySelector('[data-trouble-first]').onclick=()=>troubleFiles();pane.querySelector('[data-trouble-back]').onclick=()=>troubleFiles(previous.at(-1),previous.slice(0,-1));pane.querySelector('[data-trouble-next]').onclick=()=>troubleFiles(page.nextCursor,[...previous,cursor].slice(-20));
        pane.querySelectorAll('[data-trouble-file]').forEach(button=>button.onclick=()=>troubleSessions(page.files[Number(button.dataset.troubleFile)]));
        pane.querySelectorAll('[data-trouble-sort]').forEach(button=>button.onclick=()=>{const key=button.dataset.troubleSort;troubleDirection=troubleSort===key?(troubleDirection==='asc'?'desc':'asc'):(key==='path'?'asc':'desc');troubleSort=key;troubleFiles();});
      }catch(error){if(disposed||current!=='trouble'||ticket!==detailTicket)return;pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button data-trouble-first>Retry from first page</button>`;pane.querySelector('[data-trouble-first]').onclick=()=>troubleFiles();}
    }
    async function troubleSessions(file,cursor='',previous=[]){
      const ticket=++detailTicket;current='trouble';pane.innerHTML=banner+'<p>Loading sessions that touched this file…</p>';
      try{
        const page=await client.troubleFileSessions(file,{cursor,limit:25});if(disposed||current!=='trouble'||ticket!==detailTicket)return;
        pane.innerHTML=banner+`<h2>${escape(file.path)}</h2><p>${page.total} sessions in the current file-specific snapshot. Outcomes may have changed since the file list was loaded.</p>${page.sessions.map((s,i)=>`<button data-trouble-session="${i}">${escape(s.id)} · ${s.bad?'Recorded bad outcome':s.unknown?'Outcome uncertain':'No recorded bad outcome'} · ${escape(s.lastTouched)}</button>`).join('<br>')}<p><button data-trouble-list>Back to Trouble Files</button><button data-trouble-first>Refresh sessions</button><button data-trouble-back ${previous.length?'':'disabled'}>Previous sessions</button><button data-trouble-next ${page.nextCursor?'':'disabled'}>Next sessions</button></p>`;
        pane.querySelector('[data-trouble-list]').onclick=()=>troubleFiles();pane.querySelector('[data-trouble-first]').onclick=()=>troubleSessions(file);pane.querySelector('[data-trouble-back]').onclick=()=>troubleSessions(file,previous.at(-1),previous.slice(0,-1));pane.querySelector('[data-trouble-next]').onclick=()=>troubleSessions(file,page.nextCursor,[...previous,cursor].slice(-20));pane.querySelectorAll('[data-trouble-session]').forEach(button=>button.onclick=()=>detail(page.sessions[Number(button.dataset.troubleSession)].id));
      }catch(error){if(disposed||current!=='trouble'||ticket!==detailTicket)return;pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button data-trouble-list>Back to Trouble Files</button>`;pane.querySelector('[data-trouble-list]').onclick=()=>troubleFiles();}
    }
    async function fleetUndoHistory(cursor='',previous=[],project=undoProject) {
	  undoProject=project;
      stopStory();const ticket=++detailTicket;current='graveyard';selectingSession('');
      const controller=undoController=new AbortController();pane.innerHTML=banner+'<p role="status">Loading fleet undo history…</p>';
      try {
        const [page,summary]=await Promise.all([client.fleetGitUndos({cursor,project,limit:25,signal:controller.signal}),client.gitUndoSummary({signal:controller.signal})]);
        if(disposed||current!=='graveyard'||ticket!==detailTicket)return;
        pane.innerHTML=banner+'<button id="v2UndoGroups">Rank project folders</button>'+fleetUndoMarkup(page,summary)+`<button id="v2FleetUndoFirst">Reload first undo page</button><button id="v2FleetUndoPrevious" ${previous.length?'':'disabled'}>Previous undo page</button><button id="v2FleetUndoNext" ${page.nextCursor?'':'disabled'}>Next undo page</button><p>Previous retains the latest 20 page positions. No history is deleted.</p>`;
        $('v2UndoGroups').onclick=()=>fleetUndoProjects();
        $('v2FleetUndoFirst').onclick=()=>fleetUndoHistory();$('v2FleetUndoPrevious').onclick=()=>fleetUndoHistory(previous.at(-1),previous.slice(0,-1));$('v2FleetUndoNext').onclick=()=>fleetUndoHistory(page.nextCursor,[...previous,cursor].slice(-20));
        pane.querySelectorAll('[data-fleet-undo-record]').forEach(button=>button.onclick=()=>{const e=page.events[Number(button.dataset.fleetUndoRecord)];detail(e.sessionId,0,e.sessionSnapshot,e.sequence);});
		$('v2UndoProjectForm').onsubmit=event=>{event.preventDefault();return fleetUndoHistory('',[],$('v2UndoProject').value);};
		$('v2UndoAllProjects').onclick=()=>fleetUndoHistory('',[],null);
		pane.querySelectorAll('[data-undo-project]').forEach(button=>button.onclick=()=>fleetUndoHistory('',[],page.events[Number(button.dataset.undoProject)].project));
		pane.querySelectorAll('[data-fleet-prior-edits]').forEach(button=>button.onclick=()=>{const e=page.events[Number(button.dataset.fleetPriorEdits)];priorUndoEdits(e.sessionId,e.sequence,e.sessionSnapshot);});
      } catch(error) {
        if(disposed||current!=='graveyard'||ticket!==detailTicket)return;
        controller.abort();pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button id="v2FleetUndoFirst">Reload first undo page</button>`;
        $('v2FleetUndoFirst').onclick=()=>fleetUndoHistory();
      }
    }
    async function undoHistory(id=selectedSession,after=0,snapshot='',previous=[]) {
      stopStory();const ticket=++detailTicket;current='undos';
      if(!id){pane.innerHTML=banner+'<p>Open a session first to view undo history.</p>';return;}
      selectingSession(id);const controller=undoController=new AbortController();pane.innerHTML=banner+'<p role="status">Loading undo history…</p>';
      try{
        const [row,page]=await Promise.all([client.session(id),client.gitUndos(id,{after,snapshot,limit:25,signal:controller.signal})]);
        if(disposed||current!=='undos'||ticket!==detailTicket)return;
        readySessionExport(row,id);
        pane.innerHTML=banner+`<h2>Session undo history · ${escape(row.metadata.name||row.title||id)}</h2>${undoHistoryMarkup(page)}<button id="v2UndoFirst">Reload first undo page</button><button id="v2UndoPrevious" ${previous.length?'':'disabled'}>Previous undo page</button><button id="v2UndoNext" ${page.nextSequence?'':'disabled'}>Next undo page</button><p>Previous retains the latest 20 page positions. First returns to the beginning; no history is deleted.</p><button id="v2UndoHistory">Back to session history</button>`;
        $('v2UndoFirst').onclick=()=>undoHistory(id);$('v2UndoPrevious').onclick=()=>undoHistory(id,previous.at(-1),page.snapshot,previous.slice(0,-1));$('v2UndoNext').onclick=()=>undoHistory(id,page.nextSequence,page.snapshot,[...previous,after].slice(-20));$('v2UndoHistory').onclick=()=>detail(id);
        pane.querySelectorAll('[data-undo-record]').forEach(button=>button.onclick=()=>detail(id,0,page.snapshot,Number(button.dataset.undoRecord)));
        pane.querySelectorAll('[data-prior-edits]').forEach(button=>button.onclick=()=>priorUndoEdits(id,Number(button.dataset.priorEdits),page.snapshot));
      }catch(error){
        if(disposed||current!=='undos'||ticket!==detailTicket)return;
        selectingSession(id);pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button id="v2UndoRetry">Reload first undo page</button><button id="v2UndoHistory">Back to session history</button>`;
        $('v2UndoRetry').onclick=()=>undoHistory(id);$('v2UndoHistory').onclick=()=>detail(id);
      }
    }
    async function waterfall(id=selectedSession,after=0,snapshot='',previous=[]) {
      stopStory();const ticket=++detailTicket;current='waterfall';
      if(!id){pane.innerHTML=banner+'<p>Open a session first to view Waterfall.</p>';return;}
      selectingSession(id);pane.innerHTML=banner+'<p>Loading tool spans…</p>';
      try{
        const [row,page]=await Promise.all([client.session(id),client.toolSpans(id,{after,snapshot,limit:100})]);
        if(disposed||current!=='waterfall'||ticket!==detailTicket)return;
        readySessionExport(row,id);
        pane.innerHTML=banner+`<h2>Session waterfall · ${escape(row.metadata.name||row.title||id)}</h2>${waterfallMarkup(page.spans)}<button id="v2WaterfallFirst">First span page</button><button id="v2WaterfallPrevious" ${previous.length?'':'disabled'}>Previous span page</button><button id="v2WaterfallNext" ${page.nextSequence?'':'disabled'}>Next span page</button><p>Previous retains the latest 20 page positions. First always returns to the beginning; no history is deleted.</p><button id="v2WaterfallHistory">Open session history</button>`;
        $('v2WaterfallFirst').onclick=()=>waterfall(id);
        $('v2WaterfallPrevious').onclick=()=>waterfall(id,previous.at(-1),page.snapshot,previous.slice(0,-1));
        $('v2WaterfallNext').onclick=()=>waterfall(id,page.nextSequence,page.snapshot,[...previous,after].slice(-20));
        $('v2WaterfallHistory').onclick=()=>detail(id);
        pane.querySelectorAll('[data-waterfall-record]').forEach(button=>button.onclick=()=>detail(id,0,page.snapshot,Number(button.dataset.waterfallRecord)));
      }catch(error){
        if(disposed||current!=='waterfall'||ticket!==detailTicket)return;
        selectingSession(id);pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button id="v2WaterfallRetry">Reload first span page</button>`;
        $('v2WaterfallRetry').onclick=()=>waterfall(id);
      }
    }
    function returnFromDetail() {
      undoController?.abort();
      detailTicket++;
      current=detailReturnView;
      if(current==='calendar')renderCalendar(calendarModel);
      else if(current==='projects'){projectsModel.emit();projectsModel.load();}
      else if(current==='fingerprints'){fingerprintsModel.emit();fingerprintsModel.load();}
      else if(current==='constellation'){galaxyModel.emit();galaxyModel.load();}
      else if(current==='divergence'){divergenceModel.emit();}else if(current==='dejavu'){dejaModel.emit();}
      else if(current==='flows'){flowsWork();}else if(current==='leaks'){secretsWork();}else if(current==='unsaved'){unsavedWork();}else if(current==='trouble'){troubleFiles();}
      else if(current==='graveyard'){fleetUndoHistory();}
      else if(current==='hookprops'){hooksModel.emit();}
      else {model.emit();model.load();}
    }
    async function detail(id, after = 0, snapshot = '', anchor = null, direction = 'around') {
      stopStory();
      hooksModel?.pause();
      fingerprintsModel?.pause();
      galaxyModel?.pause();
      dejaModel?.pause();
      if(current==='calendar')detailReturnView='calendar';
      else if(current==='projects')detailReturnView='projects';
      else if(current==='divergence'||current==='flows'||current==='leaks'||current==='unsaved'||current==='trouble'||current==='graveyard'||current==='hookprops'||current==='dejavu'||current==='fingerprints'||current==='constellation')detailReturnView=current;
      else if(!['detail','timeline','story','lanes','waterfall','costflow','board','undos'].includes(current))detailReturnView='table';
      const returnLabel=detailReturnView==='divergence'?'Back to Divergence':detailReturnView==='flows'?'Back to Flows':detailReturnView==='leaks'?'Back to Secrets':detailReturnView==='unsaved'?'Back to Unsaved Work':detailReturnView==='trouble'?'Back to Trouble Files':detailReturnView==='graveyard'?'Back to Graveyard':detailReturnView==='hookprops'?'Back to Hook ideas':detailReturnView==='calendar'?'Back to calendar':detailReturnView==='projects'?'Back to projects':detailReturnView==='fingerprints'?'Back to fingerprints':detailReturnView==='constellation'?'Back to galaxy':detailReturnView==='dejavu'?'Back to Deja Vu':'Back to table';
      selectingSession(id);
      const ticket = ++detailTicket;
      current = 'detail'; pane.innerHTML = banner+'<p>Loading history…</p>';
      try {
        const history = historyPage(client,id,{after,snapshot,anchor,direction});
        const [row, page] = await Promise.all([client.session(id), history]);
        if (current!=='detail' || ticket !== detailTicket) return;
        readySessionExport(row,id);
        pane.innerHTML=banner+`<button id="v2Return">Back to table</button><h2>${escape(row.metadata.name||row.title||id)}</h2><a href="${escape(client.rawSourceURL(row.sourceId,row.generation))}" download>Download complete source</a><p>Indexed history may contain abbreviated text. Source download preserves the complete captured bytes.</p><div>${page.events.map(e=>`<article class="ev"><strong>${escape(e.kind)}</strong> ${escape(e.timestamp)}<pre>${escape(e.text)}</pre></article>`).join('')||'<p>No indexed events.</p>'}</div><button id="v2HistoryFirst">First history page</button><button id="v2HistoryPrevious" ${page.hasPrevious&&page.events.length?'':'disabled'}>Previous history page</button><button id="v2More" ${page.nextSequence?'':'disabled'}>Next history page</button>`;
        const timelineButton=document.createElement('button');timelineButton.textContent='View session timeline';timelineButton.onclick=()=>timeline(id,after,page.snapshot);pane.insertBefore(timelineButton,$('v2More'));
        const undoButton=document.createElement('button');undoButton.textContent='View session undo history';undoButton.onclick=()=>undoHistory(id);pane.insertBefore(undoButton,$('v2More'));
        const boardButton=document.createElement('button');boardButton.textContent='View agent board';boardButton.onclick=()=>board(id);pane.insertBefore(boardButton,$('v2More'));
        const exportLink=document.createElement('a');exportLink.href=client.indexedExportURL(id);exportLink.download='indexed-history.jsonl';exportLink.textContent='Download indexed history (.jsonl; may contain abbreviated text)';pane.insertBefore(exportLink,$('v2More'));
        const bundleLink=document.createElement('a');bundleLink.href=client.replayBundleURL(id);bundleLink.download='amc-replay.zip';bundleLink.textContent='Download replay package (.zip: viewer, indexed history, complete captured source)';pane.insertBefore(document.createElement('br'),$('v2More'));pane.insertBefore(bundleLink,$('v2More'));
        const bundleHelp=document.createElement('p');bundleHelp.textContent='Extract the package, open replay.html, then select indexed-history.jsonl. It works offline. The package contains private chat history. If the session changes during export, the download is incomplete; retry once it is quiet.';pane.insertBefore(bundleHelp,$('v2More'));
        const replayLink=document.createElement('a');replayLink.href='/replay.html';replayLink.download='amc-offline-replay.html';replayLink.textContent='Download offline indexed replay viewer (open it, then select the JSONL export)';pane.insertBefore(document.createElement('br'),$('v2More'));pane.insertBefore(replayLink,$('v2More'));
        const relations=document.createElement('section');
        const showRelations=document.createElement('button');showRelations.textContent='Show recorded relationships';relations.appendChild(showRelations);pane.appendChild(relations);
        let relationTicket=0;
        async function loadRelations(cursor='') {
          const mine=++relationTicket;relations.textContent='Loading relationships…';
          try {
            const related=await client.lineage(id,{limit:100,cursor});
            if(disposed||current!=='detail'||ticket!==detailTicket||mine!==relationTicket)return;
            relations.innerHTML=lineageMarkup(related);
            relations.querySelectorAll('[data-related-session]').forEach(el=>el.onclick=()=>detail(el.dataset.relatedSession));
            $('v2LineageFirst').onclick=()=>loadRelations();$('v2LineageNext').onclick=()=>loadRelations(related.nextCursor);
          } catch(error) {
            if(disposed||current!=='detail'||ticket!==detailTicket||mine!==relationTicket)return;
            relations.textContent='Relationships unavailable: '+error.message;
            const retry=document.createElement('button');retry.textContent='Retry relationships';retry.onclick=()=>loadRelations(cursor);relations.appendChild(retry);
          }
        }
        showRelations.onclick=()=>loadRelations();
        const accounting=document.createElement('section'),showAccounting=document.createElement('button');
        showAccounting.textContent='Show agent and model accounting';accounting.appendChild(showAccounting);pane.appendChild(accounting);
        let accountingTicket=0;
        async function loadAccounting(kind='agents',cursor='') {
          const mine=++accountingTicket;accounting.textContent='Loading recorded accounting…';
          try {
            const [breakdown,stats]=await Promise.all([client[kind](id,{limit:100,cursor}),client.stats(id)]);
            if(disposed||current!=='detail'||ticket!==detailTicket||mine!==accountingTicket)return;
            accounting.innerHTML=breakdownMarkup(kind,breakdown,stats);
            $('v2BreakdownAgents').onclick=()=>loadAccounting('agents');$('v2BreakdownModels').onclick=()=>loadAccounting('models');
            $('v2BreakdownFirst').onclick=()=>loadAccounting(kind);$('v2BreakdownNext').onclick=()=>loadAccounting(kind,breakdown.nextCursor);
          } catch(error) {
            if(disposed||current!=='detail'||ticket!==detailTicket||mine!==accountingTicket)return;
            accounting.textContent='Recorded accounting unavailable: '+error.message;
            const retry=document.createElement('button');retry.textContent='Retry accounting';retry.onclick=()=>loadAccounting(kind,cursor);accounting.appendChild(retry);
          }
        }
        showAccounting.onclick=()=>loadAccounting();
        const contributions=document.createElement('section'),showContributions=document.createElement('button');
        showContributions.textContent='Inspect usage contributions';contributions.appendChild(showContributions);pane.appendChild(contributions);
        let contributionTicket=0;
        async function loadContributions(cursor='',snapshot='') {
          const mine=++contributionTicket;contributions.textContent='Loading contribution evidence…';
          try {
            const result=await client.contributions(id,{limit:100,cursor,snapshot});
            if(disposed||current!=='detail'||ticket!==detailTicket||mine!==contributionTicket)return;
            contributions.innerHTML=contributionMarkup(result);
            $('v2ContributionsFirst').onclick=()=>loadContributions();
            $('v2ContributionsNext').onclick=()=>loadContributions(result.nextCursor,result.snapshot);
            contributions.querySelectorAll('[data-contribution-owner]').forEach(el=>el.onclick=()=>detail(el.dataset.contributionOwner));
          }catch(error){
            if(disposed||current!=='detail'||ticket!==detailTicket||mine!==contributionTicket)return;
            contributions.textContent='Contribution evidence unavailable: '+error.message;
            const retry=document.createElement('button');retry.textContent='Reload first contribution page';retry.onclick=()=>loadContributions();contributions.appendChild(retry);
          }
        }
        showContributions.onclick=()=>loadContributions();
        if(row.provider==='codex'&&row.forkedFromId){
          const fork=document.createElement('section'),review=document.createElement('button');
          review.textContent='Review inherited fork usage';fork.appendChild(review);pane.appendChild(fork);
          const active=()=>!disposed&&current==='detail'&&ticket===detailTicket;
          review.onclick=async()=>{
            review.disabled=true;
            try{
              const evidence=await client.inheritance(id);
              if(!active())return;
              fork.replaceChildren();
              const explanation=document.createElement('p');fork.appendChild(explanation);
              if(evidence.state!=='matched-baseline'){
                explanation.textContent='No correction available: '+(evidence.reason||'Evidence is incomplete.');
                review.disabled=false;fork.appendChild(review);return;
              }
              const match=evidence.match;
              explanation.textContent='Verified inherited baseline — input / cache read / cache write / output: '+[match.tokensIn,match.tokensCache,match.tokensCacheWrite,match.tokensOut].join(' / ')+'. Applying excludes this baseline from contribution totals only. Recorded history, later child usage, and prices remain unchanged.';
              const source=document.createElement('button');source.textContent='Open parent evidence';source.onclick=()=>detail(match.parentSessionId);fork.appendChild(source);
              const apply=document.createElement('button');apply.textContent='Apply verified baseline correction';fork.appendChild(apply);
              const status=document.createElement('p');status.setAttribute('role','status');fork.appendChild(status);
              apply.onclick=async()=>{
                apply.disabled=true;status.textContent='Applying correction…';
                try{
                  const result=await client.reconcileInheritance(id,match.parentSessionId);
                  if(!active())return;
                  status.textContent=result.selected?'Correction saved. Original recorded tokens are unchanged.':'No correction applied; evidence no longer matches.';
                  await loadContributions();
                }catch(error){
                  if(!active())return;
                  status.textContent=error.message+' Review contributions or retry; retries do not double-apply.';
                  apply.disabled=false;
                }
              };
            }catch(error){
              if(!active())return;
              fork.replaceChildren();const status=document.createElement('p');status.textContent=error.message;fork.appendChild(status);review.disabled=false;fork.appendChild(review);
            }
          };
        }
        $('v2Return').textContent=returnLabel;
        $('v2Return').onclick=returnFromDetail;
        $('v2More').onclick=()=>detail(id,page.nextSequence,page.snapshot);
        $('v2HistoryPrevious').onclick=()=>detail(id,0,page.snapshot,page.events[0].sequence,'before');
        $('v2HistoryFirst').onclick=()=>detail(id);
      } catch(error) { if (ticket !== detailTicket || current !== 'detail') return; selectingSession(id); pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p><button id="v2HistoryFirst">Reload first history page</button><button id="v2Return">${returnLabel}</button>`; $('v2HistoryFirst').onclick=()=>detail(id);$('v2Return').onclick=returnFromDetail; }
    }
    let searchTicket = 0;
    async function searchHistory(text, after = 0, snapshot = '') {
      if (!client) return;
      const ticket = ++searchTicket;
      $('soResults').textContent = 'Searching indexed history…';
      try {
        const page = await client.search(text, {after,limit:100,snapshot});
        if (ticket !== searchTicket) return;
        $('soResults').innerHTML = `<div class="so-count">${page.events.length} matches on this page of indexed history</div>` + page.events.map(e => `<div class="so-hit" data-session="${escape(e.sessionId)}" data-sequence="${e.sequence}"><div class="so-t">${escape(e.kind)} · ${escape(e.timestamp)}</div><div class="so-s">${escape(e.text)}</div></div>`).join('') + (page.nextSequence ? '<button id="v2SearchNext">Next results</button>' : '');
        $('soResults').querySelectorAll('[data-session]').forEach((el,index)=>el.onclick=()=>{
          try{const anchor=searchAnchor(page.events[index]);$('searchOverlay').classList.remove('open');detail(anchor.id,0,anchor.snapshot,anchor.sequence);}
          catch(error){$('soResults').textContent=error.message;}
        });
        if ($('v2SearchNext')) $('v2SearchNext').onclick=()=>searchHistory(text,page.nextSequence,page.snapshot);
      } catch(error) { if(ticket===searchTicket) $('soResults').textContent=error.message; }
    }
    $('soInput').onkeydown=e=>{if(e.key==='Enter') searchHistory(e.target.value.trim());};
    $('exportBtn').disabled = true;
    $('exportBtn').title = 'Open a session to export its offline replay package.';
    $('exportBtn').onclick=()=>{
      if(disposed||!client||!exportSession)return;
      const link=document.createElement('a');
      link.href=client.replayBundleURL(exportSession);link.download='amc-replay.zip';link.click();
    };
    const ready = (async()=>{
      const response=await fetch('/api/v2/bootstrap',{signal:AbortSignal.timeout(10000)});
      if(!response.ok) throw new Error('Hub bootstrap unavailable');
      const data=await response.json();
      if(disposed)return;
      client=new globalThis.AMCV2Client({hubID:data.hubId,recoveryEpoch:data.recoveryEpoch,csrf:data.csrf,onOrganization:update=>{flowsUpdate?.(update);model?.organization(update);projectsModel?.organization(update);calendarModel?.dayModel?.organization(update);},onStatus:status=>{if(status.state==='project_deletion_pending'&&projectsModel?.saving){projectsModel.deleteProgress=status.processed;projectsModel.emit();}if(model&&status.message){model.error=status.message;model.emit();}}});
      model=new TableModel(client,render);
      if(typeof notify==='function') {
        budgetAlerts=new BudgetAlerts(options=>client.usageGroups('provider',options),()=>localStorage.getItem('mc-daily-budget'),notify,{hidden:()=>document.hidden});
        sessionAlerts=new SessionAlerts(client,notify,{hidden:()=>document.hidden});
        releaseAlerts=new ReleaseAlerts(()=>client.updateCheck(),notify,{hidden:()=>document.hidden});
        runAlerts=new RunAlerts(()=>client.runActivity(),notify,{hidden:()=>document.hidden});
        collectorAlerts=new CollectorAlerts(()=>client.machines(),notify,{hidden:()=>document.hidden,afterPoll:rows=>{budgetAlerts.poll();sessionAlerts.poll();runAlerts.poll(rows);releaseAlerts.poll().then(()=>releaseAlerts.observeMachines(rows));}});
        collectorAlerts.poll();
      }
      projectsModel=new ProjectsModel(client,renderProjects);
      fingerprintsModel=new FingerprintsModel(client,renderFingerprints);
      dejaModel=new DejaModel(client,renderDeja);
      divergenceModel=new DivergenceModel(client,renderDivergence);
      hooksModel=new HookIdeasModel(client,renderHooks);
      hooksModel.secret=new globalThis.AMCV2SecretHook.Controller(client,()=>hooksModel.emit());
      galaxyModel=new FingerprintsModel(client,state=>{if(current==='constellation')renderFingerprints(state);});
      ringsModel=new RingsModel(client,renderRings);
      usageModel=new UsageModel(client,renderUsage);
      calendarModel=new CalendarModel(client,renderCalendar);rhythmModel=new RhythmModel(client,renderRhythm);
      brainDraft=new BrainDraft(client,renderBrain);
      try{recoverBookOperation();}catch(error){bookError=error.message;}
      $('appVersion').textContent=data.version;
      $('liveLabel').textContent='Go backend';
      live = new LiveRefresh({ head: () => client.changeHead(), reload: async () => {
        if(current==='projects'){
          if(document.hidden||document.querySelector('dialog[open]')||projectsModel.loading||projectsModel.saving)return false;
          if(!await projectsModel.refresh())throw new Error(projectsModel.error||'Project refresh interrupted; retrying.');
          return true;
        }
        if(current!=='table')return true;
        if (model.loading) return false;
        if (!await model.load()) throw new Error(model.error || 'Page refresh interrupted; retrying.');
        return true;
      }, hidden: () => document.hidden || !['table','projects'].includes(current), onError: error => { const active=current==='projects'?projectsModel:model;active.error=error.message;active.emit(); } });
      await live.start();
      if(['fleet','machines'].includes(current))await fleet();
      if(current==='projects')await projectsModel.load();
      if(current==='fingerprints')await fingerprintsModel.load();
      if(current==='divergence')divergenceModel.emit();
      if(current==='dejavu')dejaModel.emit();
      if(current==='hookprops')await hooksModel.load();
      if(current==='flows')await flowsWork();else if(current==='leaks')await secretsWork();else if(current==='unsaved')await unsavedWork();else if(current==='trouble')await troubleFiles();
      else if(current==='graveyard')await fleetUndoHistory();
      if(current==='constellation')await galaxyModel.load();
      if(current==='rings')await ringsModel.load();
      if(current==='usage')await usageModel.load();
      if(current==='calendar')await calendarModel.load();if(current==='rhythm')await rhythmModel.load();
      if(current==='brain')await brain();
      if(current==='audit')await audit();
      if(current==='playbooks')await playbooks();
      if(current==='economics')await economics();
      if((current==='story'||current==='lanes')&&storyPlaying)return;
      if(current==='timeline'||current==='story'||current==='lanes')await timeline(selectedSession,0,'',null,current);
      if(current==='board')await board();
    })().catch(error=>{pane.innerHTML=banner+`<p role="alert">${escape(error.message)}</p>`;});
    window.addEventListener('beforeunload',e=>{if(brainDraft?.dirty||brainDraft?.saving||bookDirty()||bookPending||plantPending||plantBusy||plantDraft.title||plantDraft.body||plantDraft.targets.length){e.preventDefault();e.returnValue='';}});
    window.addEventListener('pagehide',()=>{flowsController?.abort();secretsController?.abort();unsavedController?.abort();undoController?.abort();stopStory();hooksModel?.close();disposed=true;detailTicket++;brainDraft?.close();clearTimeout(fleetTimer);clearTimeout(searchTimer);live?.close();model?.close();projectsModel?.close();fingerprintsModel?.close();galaxyModel?.close();ringsModel?.close();usageModel?.close();calendarModel?.close();rhythmModel?.close();dejaModel?.close();divergenceModel?.close();client?.close();});
    return {ready,openSession:id=>detail(id),show(view){ flowsController?.abort();secretsController?.abort();unsavedController?.abort();undoController?.abort();hooksModel?.pause();dejaModel?.pause();divergenceModel?.pause();fingerprintsModel?.pause();galaxyModel?.pause();ringsModel?.pause(); if(view==='costflow'){hideOtherPanes();costflow();return;} detailTicket++;current=view;clearTimeout(fleetTimer);hideOtherPanes();if(view==='divergence'){divergenceModel?.emit();}else if(view==='flows'){flowsWork();}else if(view==='leaks'){secretsWork();}else if(view==='unsaved'){unsavedWork();}else if(view==='trouble'){troubleFiles();}else if(view==='graveyard'){fleetUndoHistory();}else if(view==='hookprops'){hooksModel?.load();}else if(view==='dejavu'){dejaModel?.emit();}else if(view==='rings'){ringsModel?.emit();ringsModel?.load();}else if(view==='constellation'){galaxyModel?.emit();galaxyModel?.load();}else if(view==='fingerprints'){fingerprintsModel?.emit();fingerprintsModel?.load();}else if(view==='projects'){projectsModel?.emit();projectsModel?.load();}else if(view==='table'){model?.emit();model?.load();}else if(view==='fleet'||view==='machines'){pane.innerHTML=banner+'<p>Loading fleet health…</p>';fleet();}else if(view==='usage'){pane.innerHTML=banner+'<p>Loading usage…</p>';usageModel?.load();}else if(view==='calendar'){pane.innerHTML=banner+'<p>Loading calendar…</p>';calendarModel?.load();}else if(view==='rhythm'){renderRhythm(rhythmModel);rhythmModel?.load();}else if(view==='brain'){renderBrain();brain();}else if(view==='audit'){audit();}else if(view==='playbooks'){renderPlaybooks();playbooks(bookAfter);}else if(view==='economics'){economics();}else if(view==='timeline'||view==='story'||view==='lanes'){timeline(selectedSession,0,'',null,view);}else if(view==='waterfall'){waterfall();}else if(view==='undos'){undoHistory();}else if(view==='board'){board();}else pane.innerHTML=banner+`<p>${escape(view)} is not yet ported to the candidate. Its feature gate remains pending.</p>`; }};
  }
  return { preserveViewPosition, briefPricing, RunAlerts, ReleaseAlerts, SessionAlerts, BudgetAlerts, CollectorAlerts, modelHookJSON, modelHookMarkup, undoHookJSON, undoHookMarkup, machineNetworkMarkup, machineVersionMarkup, divergenceOutcomeMarkup, alignTraceSteps, divergenceTraceMarkup, DivergenceModel, divergenceMarkup, flowRolesMarkup, flowsMarkup, secretsMarkup, unsavedMarkup, fleetUndoMarkup, undoHistoryMarkup, HookIdeasModel, hookIdeasMarkup, javascriptHookJSON, dejaResultLabel, DejaModel, dejaMarkup, galaxyFilters, galaxyNodes, galaxyMarkup, wireGalaxy, RingsModel, ringsMarkup, FingerprintsModel, fingerprintsMarkup, ProjectPicker, ProjectsModel, projectsMarkup, costFlowMarkup, RhythmModel, rhythmMetric, rhythmOvernight, rhythmMarkup, CalendarModel, calendarRange, calendarMarkup, projectFilterMarkup, agentToolsMarkup, agentActivityMarkup, boardMarkup, storyMarkup, lanesMarkup, waterfallMarkup, timelineMarkup, economicsSamplerMarkup, economicsHistoryCharts, MeasurementDraft, economicsResolutionMarkup, legacyEconomicsMarkup, economicsHistoryMarkup, economicsModelsMarkup, groupPriceCells, economicsLifetimeMarkup, economicsCostMarkup, standingOrderMarkup, BrainDraft, TableModel, UsageModel, usageChart, LiveRefresh, OrganizationDraft, pricingDisplay, coverageSummary, policyStatus, fleetCards, historyPage, lineageMarkup, searchAnchor, breakdownMarkup, boot };
});
