/* Bounded, explicitly requested local evidence for copy-only Hook Ideas. */
(function(root,factory){
  const api=factory();
  if(typeof module==='object'&&module.exports)module.exports=api;
  else root.AMCV2SecretHook=api;
})(typeof globalThis==='object'?globalThis:this,function(){
  'use strict';
  async function collect(client,{signal,onProgress=()=>{}}={}){
    const check=()=>{if(signal?.aborted)throw new DOMException('Secret evidence scan cancelled.','AbortError');};
    check();
    const identity=await client.localIdentity({signal});check();
    if(identity.state!=='configured')throw new Error('Configure the local desktop machine identity before scanning. No files were read.');
    let cursor='',snapshot=null,total=null;
    const evidence={machineId:identity.machineId,contexts:0,scanned:0,unavailable:0,capped:0,examples:[],complete:false};
    const keys=[];
    do{
      check();
      const page=await client.unsavedCandidates(identity.machineId,{cursor,limit:20,signal});check();
      if(snapshot===null){snapshot=page.snapshot;total=page.totalFiles;}
      if(page.machineId!==identity.machineId||page.snapshot!==snapshot||page.totalFiles!==total||
        !Number.isSafeInteger(total)||total<0||!Array.isArray(page.files)||page.files.length>20||
        evidence.contexts+page.files.length>total||page.nextCursor&&(!page.files.length||page.nextCursor===cursor)){
        throw new Error('Candidate history changed or its pagination is inconsistent. Restart the evidence scan.');
      }
      if(page.files.length){
        const result=await client.checkLocalSecrets(identity.machineId,page.files,{signal});check();
        if(result.machineId!==identity.machineId||result.files.length!==page.files.length)throw new Error('Local scan did not match its candidate page.');
        for(const file of result.files){
          evidence.contexts++;
          if(file.state!=='scanned'){evidence.unavailable++;continue;}
          evidence.scanned++;if(file.capped)evidence.capped++;
          // Two distinct reported locations establish the legacy threshold.
          // Never retain fragments, file bodies or a corpus-sized dedup set.
          for(const finding of file.findings){
            if(keys.length===2)break;
            const path=/^[A-Za-z]:[\\/]/.test(file.path)?file.path.replaceAll('\\','/').toLowerCase():file.path;
            const key=JSON.stringify([path,finding.line,finding.kind]);
            if(!keys.includes(key)){
              keys.push(key);evidence.examples.push({path:file.path,line:finding.line,kind:finding.kind});
            }
          }
        }
      }
      onProgress({contexts:evidence.contexts,total,scanned:evidence.scanned,unavailable:evidence.unavailable,capped:evidence.capped});
      cursor=page.nextCursor||'';
    }while(cursor);
    if(evidence.contexts!==total)throw new Error('Candidate history ended before all file contexts were checked.');
    // Detect catalog changes during a final (or single-page) filesystem check.
    const current=await client.unsavedCandidates(identity.machineId,{limit:1,signal});check();
    if(current.snapshot!==snapshot||current.totalFiles!==total||current.machineId!==identity.machineId)throw new Error('Candidate history changed during the scan. Restart the evidence scan.');
    evidence.complete=true;evidence.snapshot=snapshot;evidence.proposed=keys.length===2;
    return evidence;
  }
  class Controller{
    constructor(client,render=()=>{},run=collect){this.client=client;this.render=render;this.run=run;this.ticket=0;this.busy=false;this.error='';this.data=null;this.progress=null;}
    cancel(){this.ticket++;this.controller?.abort();this.busy=false;this.data=null;this.progress=null;}
    close(){this.cancel();this.closed=true;}
    emit(){if(!this.closed)this.render(this);}
    async start(){
      if(this.closed||this.busy)return false;
      const ticket=++this.ticket,controller=this.controller=new AbortController();
      const active=()=>!this.closed&&ticket===this.ticket;
      this.busy=true;this.error='';this.data=null;this.progress=null;this.emit();
      try{
        await this.batch;if(!active())return false;
        const pending=this.run(this.client,{signal:controller.signal,onProgress:value=>{if(active()){this.progress=value;this.emit();}}});
        this.batch=pending.catch(()=>{});
        const data=await pending;if(!active())return false;
        this.data=data;return true;
      }catch(error){if(active())this.error=error.message;return false;}
      finally{if(active()){this.busy=false;this.emit();}}
    }
  }
  const escape=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  function hookJSON(){
    const command="node -e \"let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{let i={};try{i=JSON.parse(s).tool_input||{}}catch(e){}const t=[i.content,i.new_string].filter(x=>typeof x==='string').join('\\n');if(!t||t.indexOf('amc-ok')>=0)process.exit(0);const R=[[/\\b(?:AKIA|ASIA)[A-Z0-9]{16}\\b/,'an Amazon Web Services key'],[/\\bgh[pousr]_[A-Za-z0-9]{36}\\b/,'a GitHub token'],[/\\bxox[baprs]-[A-Za-z0-9]{10,48}-[A-Za-z0-9]{10,48}/,'a Slack token'],[/\\b[sr]k_live_[A-Za-z0-9]{24,64}\\b/,'a Stripe live payment key'],[/\\bAIza[A-Za-z0-9_-]{35}\\b/,'a Google API key'],[/-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----/,'a private key']];for(const r of R)if(r[0].test(t)){console.error('Stopped: this write contains something shaped like '+r[1]+'. Put it in an environment variable and reference that instead. If it is a fake one for a doc or a test, add  amc-ok  on the same line.');process.exit(2)}})\"";
    return JSON.stringify({hooks:{PreToolUse:[{matcher:'Edit|Write|MultiEdit',hooks:[{type:'command',command}]}]}},null,2);
  }
  function markup(state){
    const head='<section class="hp-card"><h3>Review key-shaped text before writes</h3><p>Optional local evidence scan. Checks current files referenced by this computer’s captured edit history, including archived sessions, within approved roots. This is not a fleet-wide scan or a reconstruction of historical file contents.</p>';
    const controls=`<button id="v2SecretHookScan" ${state.busy?'disabled':''}>Scan local evidence for this suggestion</button>${state.busy?'<button id="v2SecretHookCancel">Cancel evidence scan</button>':''}`;
    if(state.busy)return head+controls+`<p role="status">Scanning local file contexts${state.progress?`: ${escape(state.progress.contexts)} / ${escape(state.progress.total)}`:'…'}. No proposal is shown until the scan completes.</p></section>`;
    if(state.error)return head+controls+`<p role="alert">${escape(state.error)}</p><p>No proposal is shown from failed or cancelled work.</p></section>`;
    const data=state.data;
    if(!data?.complete)return head+controls+'<p>No scan has completed. Opening Hook Ideas does not read local files for this suggestion.</p></section>';
    return head+controls+`<p>${escape(data.scanned)} of ${escape(data.contexts)} recorded file contexts scanned; ${escape(data.unavailable)} unavailable or skipped; ${escape(data.capped)} had capped findings. Contexts can refer to the same physical file. Files can change during a scan.</p>`+(data.proposed?`<p>At least two distinct reported locations matched known key shapes. Matches may be test data, not valid credentials. Consider this optional Claude Code hook only after reviewing them. It is not a security boundary and does not protect Codex tools.</p><ul>${data.examples.map(e=>`<li>${escape(e.kind)} · ${escape(e.path)}:${escape(e.line)}</li>`).join('')}</ul><pre class="hp-json">${escape(hookJSON())}</pre><button id="v2SecretHookCopy">Copy key-check hook JSON</button><p>Requires Node. Review and merge into local Claude settings manually; nothing is installed or executed here. This legacy command checks top-level content/new_string only, misses nested edits and other key formats, and bypasses the entire check if amc-ok occurs anywhere in that text. Legitimate fixtures can be blocked. Never treat a clean result as proof that a file contains no secrets.</p>`:'<p>Not proposed: fewer than two distinct reported finding locations. Unavailable files, uncaptured edits and unsupported key formats remain unknown, not clean.</p>')+'</section>';
  }
  function wire(pane,state,{copy=()=>{},isActive=()=>true}={}){
    const scan=pane.querySelector('#v2SecretHookScan');if(scan)scan.onclick=()=>{if(isActive())return state.start();};
    const cancel=pane.querySelector('#v2SecretHookCancel');if(cancel)cancel.onclick=()=>{if(isActive()){state.cancel();state.emit();}};
    const button=pane.querySelector('#v2SecretHookCopy');if(button)button.onclick=async()=>{
      if(!isActive()||state.busy||state.error||!state.data?.complete||!state.data?.proposed)return;
      const ticket=state.ticket;
      try{await copy(hookJSON());if(isActive()&&ticket===state.ticket)button.textContent='Copied — nothing installed';}
      catch(error){if(isActive()&&ticket===state.ticket){state.error='Clipboard unavailable: '+error.message;state.emit();}}
    };
  }
  return {collect,Controller,hookJSON,markup,wire};
});
