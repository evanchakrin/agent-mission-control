/* Offline indexed-export reader. No network, browser storage, or source writes. */
(function(root,factory){
  const api=factory();
  if(typeof module==='object'&&module.exports)module.exports=api;
  else root.AMCReplay=api;
})(typeof globalThis==='object'?globalThis:this,function(){
  'use strict';
  const BLOCK=64*1024,MAX_RECORD=8*1024*1024;
  const cancelled=signal=>{if(signal?.aborted)throw new Error('Replay read cancelled.');};

  // Byte offsets refer to the original Blob, including CRLF and multibyte UTF-8.
  // An unfinished final line is not a complete record in this export protocol.
  async function* records(file,{offset=0,signal}={}){
    if(!file||typeof file.slice!=='function'||!Number.isSafeInteger(file.size)||file.size<0||!Number.isSafeInteger(offset)||offset<0||offset>file.size)throw new Error('Invalid replay file or offset.');
    let position=offset,start=offset,parts=[],length=0;
    const decoder=new TextDecoder('utf-8',{fatal:true});
    while(position<file.size){
      cancelled(signal);
      const bytes=new Uint8Array(await file.slice(position,Math.min(position+BLOCK,file.size)).arrayBuffer());
      cancelled(signal);
      if(!bytes.length)throw new Error('Replay file stopped before its declared end.');
      let segment=0;
      for(let i=0;i<bytes.length;i++){
        if(bytes[i]!==10)continue;
        const piece=bytes.subarray(segment,i);length+=piece.length;
        if(length>MAX_RECORD)throw new Error('Replay record exceeds the 8 MiB safety bound; retain the original export for inspection.');
        parts.push(piece);
        const joined=new Uint8Array(length);let copied=0;
        for(const part of parts){joined.set(part,copied);copied+=part.length;}
        const end=position+i+1;
        let value;
        try{value=JSON.parse(decoder.decode(joined));}catch{throw new Error(`Invalid JSON or UTF-8 at byte ${start}.`);}
        yield {value,start,end,bytes:length};
        start=end;parts=[];length=0;segment=i+1;
      }
      if(segment<bytes.length){
        const tail=bytes.slice(segment);parts.push(tail);length+=tail.length;
        if(length>MAX_RECORD)throw new Error('Replay record exceeds the 8 MiB safety bound; retain the original export for inspection.');
      }
      position+=bytes.length;
    }
    if(length)throw new Error('Truncated replay: final record has no newline.');
  }

  async function validate(file,{signal,onProgress}={}){
    let header=null,complete=false,events=0,observations=0,count=0,lastProgress=0;
    for await(const row of records(file,{signal})){
      cancelled(signal);const value=row.value;
      if(!value||typeof value!=='object'||Array.isArray(value))throw new Error('Invalid replay record.');
      if(complete)throw new Error('Unexpected records after the completion record.');
      if(!header){
        if(value.type!=='header'||value.version!==1||!/^[a-f0-9]{64}$/.test(value.boundary||'')||typeof value.session?.id!=='string'||!value.session.id)throw new Error('Unsupported or missing replay header.');
        header=value;
      }else if(value.type==='event'){
        if(!value.event||value.event.sessionId!==header.session.id)throw new Error('Replay event belongs to a different session.');
        events++;
      }else if(value.type==='usage'){
        if(!value.usage||value.usage.sessionId!==header.session.id)throw new Error('Replay usage belongs to a different session.');
        observations++;
      }else if(value.type==='complete'){
        if(value.version!==1||value.boundary!==header.boundary||(value.events??0)!==events||(value.observations??0)!==observations)throw new Error('Replay completion does not match its header and record counts.');
        complete=true;
      }else throw new Error(value.type==='error'?'The exporting hub reported an incomplete stream.':'Unsupported replay record type.');
      count++;
      if(row.end-lastProgress>=1024*1024){onProgress?.({bytes:row.end,total:file.size,events,observations});lastProgress=row.end;}
    }
    if(!header||!complete)throw new Error('Incomplete replay: required completion record is missing.');
    onProgress?.({bytes:file.size,total:file.size,events,observations});
    return {header,events,observations,records:count,bytes:file.size};
  }

  async function page(file,{offset=0,signal,limit=100}={}){
    if(!Number.isInteger(limit)||limit<1||limit>100)throw new Error('Replay pages require 1–100 records.');
    const rows=[];let nextOffset=offset,bytes=0;
    for await(const row of records(file,{offset,signal})){
      if(rows.length&&(rows.length>=limit||bytes+row.bytes>4*1024*1024))break;
      rows.push(row.value);bytes+=row.bytes;nextOffset=row.end;
    }
    return {rows,nextOffset:nextOffset<file.size?nextOffset:null,bytes};
  }
  return {records,validate,page};
});
