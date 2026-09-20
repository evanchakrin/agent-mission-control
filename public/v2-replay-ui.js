/* Fixed offline viewer bootstrap; all source-provided content is textContent. */
(function(){
  'use strict';
  const $=id=>document.getElementById(id);
  let file=null,controller=null,ticket=0,validated=false,busy=false,current=0,next=null,previous=[];
  let playing=false,timer=null,playIndex=0,playTicket=0;
  let pageRows=[];
  function pause(){playing=false;++playTicket;clearTimeout(timer);timer=null;controls();}
  function controls(){
    $('cancel').disabled=!busy;
    $('first').disabled=busy||!validated||current===0;
    $('previous').disabled=busy||!validated||previous.length===0;
    $('next').disabled=busy||!validated||next===null;
    $('play').disabled=busy||!validated||playing||!$('history').children.length;
    $('pause').disabled=!playing;
    $('view').disabled=busy||!validated;
  }
  function display(rows){
    pageRows=rows;
    playIndex=0;
    $('history').replaceChildren();
    for(const row of rows){
      const article=document.createElement('article'),heading=document.createElement('h3'),body=document.createElement('pre');
      const value=row.type==='event'?row.event:row.type==='usage'?row.usage:row;
      heading.textContent=row.type==='event'?`${row.event.kind||'Event'} · ${row.event.timestamp||'Unknown time'} · ${row.event.agentId||'Unattributed agent'}`:row.type;
      const readable=$('view').value==='conversation';
      body.textContent=readable&&row.type==='event'?(typeof value.text==='string'&&value.text?value.text:'No indexed text. Inspect the raw record and complete source for evidence.'):JSON.stringify(value,null,2);
      article.append(heading,body);$('history').append(article);
      if(readable&&row.type==='event'){
        const details=document.createElement('details'),summary=document.createElement('summary'),raw=document.createElement('pre');
        summary.textContent='Raw indexed record';raw.textContent=JSON.stringify(value,null,2);
        details.append(summary,raw);article.append(details);
      }
    }
  }
  async function advance(mine){
    if(!playing||mine!==playTicket)return;
    if(playIndex>=$('history').children.length){
      if(next===null){pause();$('status').textContent='Replay reached the end of the export.';return;}
      await show(next);
      if(!playing||mine!==playTicket)return;
      if(playIndex>=$('history').children.length){pause();return;}
    }
    for(const row of $('history').children)row.removeAttribute('aria-current');
    const row=$('history').children[playIndex++];row.setAttribute('aria-current','true');row.scrollIntoView({block:'nearest'});
    $('status').textContent=`Playing record ${playIndex} on this page. Intervals are playback controls, not original event timing.`;
    const interval=Number($('speed').value);
    timer=setTimeout(()=>advance(mine),[100,500,1000].includes(interval)?interval:1000);
  }
  async function show(offset,mode='next'){
    if(busy||!validated)return;
    const mine=++ticket;controller=new AbortController();busy=true;controls();$('status').textContent='Reading bounded history page…';
    try{
      const result=await AMCReplay.page(file,{offset,signal:controller.signal});
      if(mine!==ticket)return;
      if(mode==='first')previous=[];
      else if(mode==='previous')previous.pop();
      else if(offset!==current){previous.push(current);if(previous.length>20)previous.shift();}
      current=offset;next=result.nextOffset;display(result.rows);
      $('status').textContent=`Showing ${result.rows.length} records from byte ${offset.toLocaleString()}${next===null?' · end of export':''}.`;
    }catch(error){if(mine===ticket){pause();$('status').textContent=error.message;}}
    finally{if(mine===ticket){busy=false;controls();}}
  }
  $('file').onchange=async()=>{
    pause();
    controller?.abort();const mine=++ticket;
    file=$('file').files[0]||null;validated=false;current=0;next=null;previous=[];
    pageRows=[];$('history').replaceChildren();$('summary').textContent='';$('title').textContent='History';
    busy=!!file;controls();if(!file){$('status').textContent='No file selected.';return;}
    controller=new AbortController();$('status').textContent='Validating complete export…';
    try{
      const result=await AMCReplay.validate(file,{signal:controller.signal,onProgress:p=>{if(mine===ticket)$('status').textContent=`Validating ${p.bytes.toLocaleString()} / ${p.total.toLocaleString()} bytes…`;}});
      if(mine!==ticket)return;
      validated=true;busy=false;
      $('title').textContent=result.header.session.metadata?.name||result.header.session.title||result.header.session.id;
      $('summary').textContent=`Structurally complete export: ${result.events.toLocaleString()} indexed events, ${result.observations.toLocaleString()} usage observations. Source text can be abbreviated.`;
      await show(0,'first');
    }catch(error){if(mine===ticket){$('status').textContent=error.message;validated=false;}}
    finally{if(mine===ticket){busy=false;controls();}}
  };
  $('cancel').onclick=()=>{pause();controller?.abort();++ticket;busy=false;$('status').textContent='Read cancelled. No file was modified.';controls();};
  $('first').onclick=()=>{pause();return show(0,'first');};
  $('previous').onclick=()=>{pause();return show(previous[previous.length-1],'previous');};
  $('next').onclick=()=>{pause();return show(next);};
  $('play').onclick=()=>{if(busy||!validated||playing)return;playing=true;const mine=++playTicket;controls();return advance(mine);};
  $('pause').onclick=pause;
  $('view').onchange=()=>{if(busy||!validated)return;pause();display(pageRows);};
  document.addEventListener('visibilitychange',()=>{if(document.hidden)pause();});
})();
