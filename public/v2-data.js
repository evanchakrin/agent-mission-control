/* AMC's opt-in, bounded v2 data adapter. This file does not change the legacy
 * dashboard or intercept fetch. Wire it only after the feature parity gates. */
(function (root, factory) {
  'use strict';
  const exports = factory();
  if (typeof module === 'object' && module.exports) module.exports = exports;
  else Object.assign(root, exports);
})(typeof globalThis === 'object' ? globalThis : this, function () {
  'use strict';

  class AMCV2Error extends Error {
    constructor(message, { code = 'request_failed', status = 0, retryable = false, retryAfter = 0 } = {}) {
      super(message); this.name = 'AMCV2Error';
      Object.assign(this, { code, status, retryable, retryAfter });
    }
  }
  const clone = value => value == null ? value : JSON.parse(JSON.stringify(value));
  const stale = () => new AMCV2Error('A newer server snapshot has replaced this response.', { code: 'stale_response', retryable: true });
  const positiveLimit = (value, fallback = 100) => {
    const n = value == null ? fallback : Number(value);
    if (!Number.isInteger(n) || n < 1) throw new AMCV2Error('Page limit must be a positive integer.', { code: 'invalid_page' });
    return Math.min(n, 500);
  };
  const sequence = value => {
    const n = value == null ? 0 : Number(value);
    if (!Number.isSafeInteger(n) || n < 0) throw new AMCV2Error('Invalid history cursor.', { code: 'invalid_cursor' });
    return n;
  };
  const metadata = value => ({ archived: false, pinned: false, revision: 0, ...clone(value || {}) });
  const own = (object, key) => Object.prototype.hasOwnProperty.call(object, key);

  class AMCV2Client {
    constructor(options = {}) {
      if (!options.hubID || typeof options.hubID !== 'string') throw new AMCV2Error('A stable hub identity is required.', { code: 'missing_hub_identity' });
      this.hubID = options.hubID;
      this.baseURL = String(options.baseURL || '').replace(/\/$/, '');
      this.fetch = options.fetch || globalThis.fetch?.bind(globalThis);
      if (!this.fetch) throw new AMCV2Error('Fetch is unavailable.');
      this.storage = options.storage === undefined ? globalThis.localStorage : options.storage;
      this.onOrganization = options.onOrganization || (() => {});
      this.onStatus = options.onStatus || (() => {});
      this.getHeaders = options.getHeaders || (() => ({}));
      this.csrf = options.csrf || null;
      this.uuid = options.uuid || (() => globalThis.crypto.randomUUID());
      this.random = options.random || Math.random;
      this.now = options.now || Date.now;
      this.requestTimeout = Math.max(10, Math.min(options.requestTimeout || 10000, 30000));
      this.responseCap = Math.max(1024, Math.min(options.responseCap || 8 * 1024 * 1024, 16 * 1024 * 1024));
      this.retryBase = Math.max(10, options.retryBase || 1000);
      this.retryCap = Math.max(this.retryBase, Math.min(options.retryCap || 300000, 300000));
      this.maxCachedSessions = Math.max(1, options.maxCachedSessions || 1000);
      this.maxPendingSessions = Math.max(1, options.maxPendingSessions || 1000);
      this.maxConcurrentWrites = Math.max(1, Math.min(options.maxConcurrentWrites || 4, 8));
      this.key = 'amc:v2:' + encodeURIComponent(this.hubID);
      this.entries = new Map();
      this.requests = new Set();
      this.queries = new Map();
      this.serial = 0;
      this.activeWrites = 0;
      this.timer = null;
      this.closed = false;
      this.recoveryEpoch = options.recoveryEpoch || null;
      this.changeAfter = 0;
      this.changeController = null;
      this._load();
      if (options.autoResume !== false) queueMicrotask(() => this.resume());
    }

    _notify(callback, value) { try { callback(value); } catch { /* a rendering callback cannot damage the operation journal */ } }
    _status(state, detail = {}) { this._notify(this.onStatus, { state, hubID: this.hubID, ...detail }); }
    _entry(id) {
      if (typeof id !== 'string' || !id || id.length > 1024) throw new AMCV2Error('A session ID is required.', { code: 'invalid_session' });
      let entry = this.entries.get(id);
      if (!entry) { entry = { id, base: null, row: null, operations: [], running: false }; this.entries.set(id, entry); }
      else { this.entries.delete(id); this.entries.set(id, entry); }
      return entry;
    }
    _trim() {
      if (this.entries.size <= this.maxCachedSessions) return;
      for (const [id, entry] of this.entries) {
        if (!entry.operations.length && !entry.running) this.entries.delete(id);
        if (this.entries.size <= this.maxCachedSessions) break;
      }
    }
    organization(id) {
      const entry = this.entries.get(id);
      const result = metadata(entry?.base);
      for (const operation of entry?.operations || []) {
        Object.assign(result, clone(operation.patch));
        if (own(operation.patch, 'project')) result.projectOverride = true;
      }
      const blocked = entry?.operations.find(operation => operation.blocked);
      return { ...result, pending: !!entry?.operations.length, pendingCount: entry?.operations.length || 0,
        pendingState: blocked?.blocked || (entry?.operations.length ? 'pending' : 'acknowledged') };
    }
    _emit(id) { this._notify(this.onOrganization, { sessionID: id, metadata: this.organization(id) }); }

    _load() {
      if (!this.storage) { this._status('blocked_local_storage', { message: 'Pending changes cannot survive a restart without local storage.' }); return; }
      try {
        const saved = JSON.parse(this.storage.getItem(this.key + ':pending') || 'null');
        if (saved) {
          if (saved.version !== 1 || saved.hubID !== this.hubID || !Array.isArray(saved.entries)) throw new Error('Invalid pending journal');
          if (!this.recoveryEpoch) this.recoveryEpoch = saved.recoveryEpoch || null;
          for (const row of saved.entries) {
            const entry = this._entry(row.id);
            entry.base = saved.recoveryEpoch === this.recoveryEpoch ? row.base : null;
            if (!Array.isArray(row.operations)) throw new Error('Invalid operation list');
            entry.operations = row.operations.map(operation => {
              if (!operation.intentID || !operation.operationID) throw new Error('Invalid operation identity');
              return { ...operation, patch: this._validatePatch(operation.patch), blocked: operation.blocked || null };
            });
          }
        }
        const changes = JSON.parse(this.storage.getItem(this.key + ':changes') || 'null');
        if (changes && changes.recoveryEpoch === this.recoveryEpoch) this.changeAfter = sequence(changes.after);
      } catch {
        // Never overwrite a journal that could contain unacknowledged intent.
        this.journalBlocked = true;
        this._status('blocked_local_storage', { message: 'The pending-change journal is unreadable. Preserve it before recovery.' });
      }
    }
    _persist() {
      if (!this.storage || this.journalBlocked) throw new AMCV2Error('Pending changes cannot be durably saved in this browser.', { code: 'blocked_local_storage' });
      try {
        const entries = [...this.entries.values()].filter(entry => entry.operations.length).map(entry => ({
          id: entry.id, base: entry.base,
          operations: entry.operations.map(({ resolve, reject, ...operation }) => operation),
        }));
        if (entries.length) this.storage.setItem(this.key + ':pending', JSON.stringify({ version: 1, hubID: this.hubID, recoveryEpoch: this.recoveryEpoch, entries }));
        else this.storage.removeItem(this.key + ':pending');
      } catch (error) { throw new AMCV2Error('Pending changes could not be saved locally. They remain visibly pending.', { code: 'blocked_local_storage' }); }
    }
    _persistCursor() {
      if (!this.storage || this.journalBlocked) return;
      try { this.storage.setItem(this.key + ':changes', JSON.stringify({ recoveryEpoch: this.recoveryEpoch, after: this.changeAfter })); }
      catch { this._status('cursor_not_persisted', { message: 'Changes may replay after a browser restart; duplicate sequences are safe.' }); }
    }

    setServerIdentity(health) {
      if (health.hubId && health.hubId !== this.hubID) throw new AMCV2Error('The server identity changed. Pending changes were not sent to the replacement.', { code: 'hub_identity_mismatch' });
      const epoch = health.recoveryEpoch;
      if (typeof epoch !== 'string' || !epoch) return;
      const changed = epoch !== this.recoveryEpoch;
      if (this.recoveryEpoch && this.recoveryEpoch !== epoch) {
        for (const [id, entry] of this.entries) {
          entry.base = null; entry.row = null;
          if (!entry.operations.length && !entry.running) this.entries.delete(id);
          else this._emit(id);
        }
        this.changeAfter = 0;
        this._status('hub_restored', { message: 'The hub was restored. Refreshing its revisions while preserving pending changes.' });
      }
      this.recoveryEpoch = epoch;
      try { this._persist(); } catch (error) { this._status(error.code, { message: error.message }); }
      this._persistCursor();
      if (changed) this.resume();
    }

    async _request(path, options = {}) {
      if (this.closed) throw new AMCV2Error('The client is closed.', { code: 'closed' });
      const controller = new AbortController(); this.requests.add(controller);
      const abortFromCaller = () => controller.abort();
      if (options.signal?.aborted) controller.abort();
      else options.signal?.addEventListener('abort', abortFromCaller, { once: true });
      const timeout = setTimeout(() => controller.abort(), this.requestTimeout);
      let response;
      try {
        response = await this.fetch(this.baseURL + path, { ...options, redirect: 'error', credentials: 'same-origin',
          headers: { Accept: 'application/json', ...this.getHeaders(), ...(this.csrf ? { 'X-MC-CSRF': this.csrf } : {}), ...(options.headers || {}) }, signal: controller.signal });
        let text;
        if (response.body?.getReader) {
          const reader = response.body.getReader(); const decoder = new TextDecoder();
          let length = 0; const parts = [];
          try {
            for (;;) { const { done, value } = await reader.read(); if (done) break;
              length += value.byteLength;
              if (length > this.responseCap) { controller.abort(); throw new AMCV2Error('The server exceeded the bounded response size.', { code: 'response_too_large' }); }
              parts.push(decoder.decode(value, { stream: true }));
            }
            parts.push(decoder.decode()); text = parts.join('');
          } finally { reader.releaseLock(); }
        } else {
          text = await response.text();
          if (text.length > this.responseCap) throw new AMCV2Error('The server response is too large.', { code: 'response_too_large' });
        }
        let value = null;
        if (text) { try { value = JSON.parse(text); } catch { throw new AMCV2Error('The hub returned an incomplete or invalid response.', { code: 'invalid_response', retryable: true }); } }
        if (!response.ok) {
          const header = response.headers?.get('Retry-After');
          const seconds = Number(header);
          const retryAfter = header ? Number.isFinite(seconds) ? Math.max(0, Math.min(seconds * 1000, 86400000))
            : Math.max(0, Math.min(Date.parse(header) - this.now(), 86400000)) || 0 : 0;
          throw new AMCV2Error(value?.error || `Hub request failed (${response.status}).`, {
            status: response.status, code: value?.code || 'http_error', retryAfter,
            retryable: response.status === 408 || response.status === 429 || response.status >= 500,
          });
        }
        return value;
      } catch (error) {
        if (error instanceof AMCV2Error) throw error;
        throw new AMCV2Error(controller.signal.aborted ? 'The request timed out; its outcome will be reconciled safely.' : 'The hub is unavailable; pending changes are retained.',
          { code: this.closed ? 'closed' : controller.signal.aborted ? 'deadline' : 'network', retryable: !this.closed });
      } finally { clearTimeout(timeout); options.signal?.removeEventListener('abort', abortFromCaller); this.requests.delete(controller); }
    }

    _filters(options = {}, includePage = true) {
      const query = new URLSearchParams();
	  if(options.projectAssignment!=null){if(typeof options.projectAssignment!=='string'||new TextEncoder().encode(options.projectAssignment).byteLength>1024)throw new AMCV2Error('Invalid project assignment filter.',{code:'invalid_filter'});query.set('projectAssignment',options.projectAssignment);}
      for (const key of ['machineId', 'provider', 'project', 'q']) if (options[key] != null && options[key] !== '') {
        if (typeof options[key] !== 'string' || options[key].length > 4096) throw new AMCV2Error('Invalid text filter.', { code: 'invalid_filter' });
        query.set(key, options[key]);
      }
      for (const key of ['archived', 'unassignedProject']) if (options[key] != null) {
        if (typeof options[key] !== 'boolean') throw new AMCV2Error('Boolean filters must be booleans.', { code: 'invalid_filter' });
        query.set(key, String(options[key]));
      }
      if (options.unassignedProject && options.project) throw new AMCV2Error('Project filters conflict.', { code: 'invalid_filter' });
      if (options.sort && !['lastActivity', 'title', 'tokens', 'cost'].includes(options.sort)
        || options.direction && !['asc', 'desc'].includes(options.direction)) throw new AMCV2Error('Unsupported history-wide sort.', { code: 'invalid_sort' });
      if (includePage && options.sort) query.set('sort', options.sort);
      if (includePage && options.direction) query.set('direction', options.direction);
      if (options.pinnedFirst != null) {
        if (typeof options.pinnedFirst !== 'boolean') throw new AMCV2Error('Pinned-first ordering must be a boolean.', { code: 'invalid_sort' });
        if (includePage) query.set('pinnedFirst', String(options.pinnedFirst));
      }
      for (const key of ['from', 'to']) if (options[key] != null) {
        const raw = options[key] instanceof Date ? options[key].toISOString() : options[key];
        if (typeof raw !== 'string' || !/^\d{4}-\d{2}-\d{2}(?:T.*(?:Z|[+-]\d{2}:\d{2}))?$/.test(raw) || !Number.isFinite(Date.parse(raw))) throw new AMCV2Error('Dates must be ISO dates or timestamps with a timezone.', { code: 'invalid_filter' });
        query.set(key, raw);
      }
      if (query.has('from') && query.has('to') && Date.parse(query.get('from')) > Date.parse(query.get('to'))) throw new AMCV2Error('Date bounds are reversed.', { code: 'invalid_filter' });
      if (includePage) { query.set('limit', positiveLimit(options.limit)); if (options.cursor) query.set('cursor', String(options.cursor)); }
      return query;
    }

    _acceptSession(row, epoch) {
      if (epoch !== this.recoveryEpoch) throw stale();
      if (!row || typeof row.id !== 'string') throw new AMCV2Error('Invalid session response.', { code: 'invalid_response', retryable: true });
      const entry = this._entry(row.id); const next = metadata(row.metadata);
      if (!Number.isSafeInteger(next.revision) || next.revision < 0) throw new AMCV2Error('Invalid organization revision.', { code: 'invalid_response', retryable: true });
      if (!entry.base || next.revision >= entry.base.revision) entry.base = next;
      const effective = this.organization(row.id);
      entry.row = { ...row, metadata: effective };
      this._emit(row.id); this._trim();
      return { ...row, metadata: effective };
    }

    async updateCheck() { return this._read('/api/v2/update-check'); }
    async runActivity() { return this._read('/api/v2/run-activity'); }
    async health() { const health = await this._request('/api/v2/health'); this.setServerIdentity(health); return health; }
    async bootstrap() {
      const data = await this._request('/api/v2/bootstrap');
      if (typeof data?.csrf === 'string') this.csrf = data.csrf;
      this.setServerIdentity(data.health ? { ...data.health, hubId: data.hubId || data.health.hubId, recoveryEpoch: data.recoveryEpoch || data.health.recoveryEpoch } : data);
      return data;
    }
    _brainID(id) {
      if (typeof id !== 'string' || !id || id.length > 8192) throw new AMCV2Error('A guidance file identity is required.', { code: 'invalid_brain_file' });
      return encodeURIComponent(id);
    }
    async brain() {
      const result = await this._read('/api/brain');
      if (!Array.isArray(result?.items) || typeof result.csrf !== 'string' || !result.csrf) throw new AMCV2Error('Invalid desktop guidance inventory.', { code: 'invalid_response' });
      this.csrf = result.csrf;
      return result;
    }
    async brainFile(id) {
      const file = await this._read('/api/brain/file?id=' + this._brainID(id));
      if (file?.id !== id || typeof file.content !== 'string' || !/^[a-f0-9]{64}$/.test(file.expectedHash || '')) throw new AMCV2Error('The desktop omitted the file identity or conflict hash.', { code: 'invalid_response' });
      return file;
    }
    async brainHistory(id, before = '') {
      if (typeof before !== 'string' || before.length > 256) throw new AMCV2Error('Invalid snapshot cursor.', { code: 'invalid_cursor' });
      const result = await this._read('/api/brain/history?id=' + this._brainID(id) + (before ? '&before=' + encodeURIComponent(before) : ''));
      if (!Array.isArray(result?.history) || result.history.length > 100 || (result.nextCursor && (typeof result.nextCursor !== 'string' || result.nextCursor === before || !result.history.length || result.history.at(-1).stamp !== result.nextCursor))) throw new AMCV2Error('Invalid snapshot history page.', { code: 'invalid_response' });
      return result;
    }
    async desktopAudit(before = '') {
      if (typeof before !== 'string' || before && !/^[1-9][0-9]{0,18}$/.test(before)) throw new AMCV2Error('Invalid audit cursor.', { code: 'invalid_cursor' });
      const page = await this._read('/api/audit?limit=100'+(before?'&before='+before:''));
      if (!Array.isArray(page?.entries) || page.entries.length > 100 || page.entries.some((row,index)=>!Number.isSafeInteger(row.id)||row.id<1||index>0&&row.id>=page.entries[index-1].id||before&&BigInt(row.id)>=BigInt(before)) || page.nextCursor && (typeof page.nextCursor!=='string'||!page.entries.length||page.nextCursor!==String(page.entries.at(-1).id))) throw new AMCV2Error('Invalid audit page.', { code: 'invalid_response' });
      return page;
    }
    async playbooks(after = '') {
      if(typeof after!=='string'||after.length>512)throw new AMCV2Error('Invalid playbook cursor.',{code:'invalid_cursor'});
      const page=await this._read('/api/playbooks?limit=10'+(after?'&after='+encodeURIComponent(after):''));
      if(!Array.isArray(page?.items)||page.items.length>10||page.items.some((item,index)=>typeof item.id!=='string'||item.id<= (index?page.items[index-1].id:after))||page.nextCursor&&(!page.items.length||page.nextCursor!==page.items.at(-1).id))throw new AMCV2Error('Invalid playbook page.',{code:'invalid_response'});
      return page;
    }
    async standingOrders(after='') {
      if(typeof after!=='string'||after.length>512)throw new AMCV2Error('Invalid standing-order cursor.',{code:'invalid_cursor'});
      const page=await this._read('/api/directives?limit=10'+(after?'&after='+encodeURIComponent(after):''));
      if(!Array.isArray(page?.items)||page.items.length>10||page.items.some((item,index)=>typeof item.id!=='string'||item.id<=(index?page.items[index-1].id:after))||page.nextCursor&&(!page.items.length||page.nextCursor!==page.items.at(-1).id))throw new AMCV2Error('Invalid standing-order page.',{code:'invalid_response'});
      return page;
    }
    _validatePlant(o){
      if(!o||o.op!=='plant'||typeof o.operationId!=='string'||!/^[A-Za-z0-9_-]{1,128}$/.test(o.operationId)||typeof o.title!=='string'||o.title.length>100||typeof o.body!=='string'||!o.body.trim()||new TextEncoder().encode(o.body).byteLength>20000||typeof o.topic!=='string'||o.topic.length>40||!Number.isInteger(o.reviewEveryDays)||o.reviewEveryDays<1||o.reviewEveryDays>3650||!Array.isArray(o.targets)||!o.targets.length||o.targets.length>40||new Set(o.targets).size!==o.targets.length||o.targets.some(id=>typeof id!=='string'||!id||id.length>8192))throw new AMCV2Error('Planting requires a title, body (up to 20,000 UTF-8 bytes), review interval and 1–40 distinct targets.',{code:'invalid_directive'});
      const body=JSON.stringify(o);
      if(new TextEncoder().encode(body).byteLength>1024*1024)throw new AMCV2Error('Planting request exceeds its bound.',{code:'invalid_directive'});
      return body;
    }
    pendingPlantOperations(){
      const storage=this.storage,prefix=this.key+':plant-op:',operations=[];
      try{
        if(!storage||typeof storage.length!=='number'||typeof storage.key!=='function')throw new Error('No storage');
        for(let i=0;i<storage.length;i++){
          const key=storage.key(i);if(typeof key!=='string'||!key.startsWith(prefix))continue;
          const raw=storage.getItem(key);if(typeof raw!=='string'||raw.length>1024*1024||operations.length>=20)throw new Error('Journal bound');
          const o=JSON.parse(raw);this._validatePlant(o);if(key!==prefix+o.operationId)throw new Error('Identity mismatch');operations.push(o);
        }
      }catch{throw new AMCV2Error('Planting recovery storage is unavailable or damaged. Preserve it before recovery.',{code:'blocked_local_storage'});}
      return operations;
    }
    async plantStandingOrder(o){
      const body=this._validatePlant(o),key=this.key+':plant-op:'+o.operationId;
      if(!this.csrf)throw new AMCV2Error('Load desktop authentication before planting.',{code:'missing_csrf'});
      try{
        const prior=this.storage?.getItem(key),pending=this.pendingPlantOperations();
        if(prior&&prior!==body||!prior&&pending.length>=20)throw new Error('Journal collision or bound');
        this.storage.setItem(key,body);
      }catch{throw new AMCV2Error('Planting could not be saved locally. No request was sent.',{code:'blocked_local_storage'});}
      const result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body});
      if(result?.ok!==true||result.operationId!==o.operationId||typeof result.id!=='string'||!result.id||result.item?.id!==result.id||!Array.isArray(result.results)||result.results.length>40)throw new AMCV2Error('Planting acknowledgment is uncertain. Preserve and retry the same operation.',{code:'invalid_response'});
      try{if(this.storage.getItem(key)===body)this.storage.removeItem(key);}catch{throw new AMCV2Error('Planting was acknowledged but local recovery storage could not be cleared. The same operation may be retried.',{code:'blocked_local_storage'});}
      return result;
    }
    async standingOrder(id){
      if(typeof id!=='string'||!id||id.length>512)throw new AMCV2Error('Standing-order identity is required.',{code:'invalid_directive'});
      const result=await this._read('/api/directives?id='+encodeURIComponent(id));
      if(result?.item?.id!==id)throw new AMCV2Error('Invalid standing-order response.',{code:'invalid_response'});
      return result.item;
    }
    async previewStandingOrderMeasurement(id,expectedHash){
      if(typeof id!=='string'||!id||id.length>512||!/^[a-f0-9]{64}$/.test(expectedHash||''))throw new AMCV2Error('Measurement preview requires the observed rule state.',{code:'invalid_directive'});
      const result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({op:'remeasure-preview',id,expectedHash})});
      if(result?.ok!==true||result.id!==id||result.stateHash!==expectedHash||!/^measure_[A-Za-z0-9_-]{1,120}$/.test(result.previewId||'')||typeof result.body!=='string'||!result.body.trim()||new TextEncoder().encode(result.body).byteLength>20000||!Number.isSafeInteger(result.measuredFrom?.sessions)||result.measuredFrom.sessions<0||!Number.isSafeInteger(result.measuredFrom?.subs)||result.measuredFrom.subs<200)throw new AMCV2Error('Measurement preview is inconsistent. No file apply was requested.',{code:'invalid_response'});
      return result;
    }
    async applyStandingOrderMeasurement(preview){
      const p=preview;
      if(!p||typeof p.id!=='string'||!p.id||p.id.length>512||!/^measure_[A-Za-z0-9_-]{1,120}$/.test(p.previewId||'')||!/^[a-f0-9]{64}$/.test(p.stateHash||'')||typeof p.body!=='string'||new TextEncoder().encode(p.body).byteLength>20000)throw new AMCV2Error('A reviewed measurement preview is required.',{code:'invalid_directive'});
      const result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({op:'remeasure',id:p.id,key:p.previewId,expectedHash:p.stateHash})});
      if(result?.ok!==true||result.id!==p.id||result.body!==p.body||typeof result.complete!=='boolean'||!Array.isArray(result.results)||result.results.length>40||result.results.some(r=>!r||typeof r.status!=='string'||r.error!==undefined&&typeof r.error!=='string'))throw new AMCV2Error('Measurement apply acknowledgement is uncertain. Refresh the rule and inspect its files.',{code:'invalid_response'});
      return result;
    }
    async guidanceRegistry(){
      const result=await this._read('/api/directives?registry=1');
      if(!Array.isArray(result?.targets)||!Array.isArray(result?.roots))throw new AMCV2Error('Invalid guidance registry.',{code:'invalid_response'});
      return result;
    }
    async changeGuidanceRoot(op,path){
      if(!['add-root','remove-root'].includes(op)||typeof path!=='string'||!path.trim()||path.length>400)throw new AMCV2Error('A full repository path of at most 400 characters is required.',{code:'invalid_root'});
      const result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({op,path})});
      if(result?.ok!==true||!Array.isArray(result.roots)||!Array.isArray(result.targets)||!Number.isSafeInteger(result.stranded)||result.stranded<0)throw new AMCV2Error('Registry acknowledgment is uncertain. Refresh the registry before another change.',{code:'invalid_response'});
      return result;
    }
    async checkStandingOrder(id){
      if(typeof id!=='string'||!id||id.length>512)throw new AMCV2Error('Standing-order identity is required.',{code:'invalid_directive'});
      const result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({op:'check',id})});
      if(!Array.isArray(result?.statuses)||result.statuses.length>40)throw new AMCV2Error('Invalid standing-order check response.',{code:'invalid_response'});
      return result;
    }
    async reviewStandingOrder(operation){
      const o=operation;
      if(!o||typeof o.id!=='string'||!o.id||o.id.length>512||typeof o.operationId!=='string'||!/^[A-Za-z0-9_-]{1,128}$/.test(o.operationId)||typeof o.expectedHash!=='string'||!/^[a-f0-9]{64}$/.test(o.expectedHash))throw new AMCV2Error('Review requires the observed rule hash and operation identity.',{code:'invalid_directive'});
      const result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({op:'reviewed',id:o.id,operationId:o.operationId,expectedHash:o.expectedHash})});
      if(result?.ok!==true||result.id!==o.id||result.operationId!==o.operationId||result.item?.id!==o.id||!Number.isSafeInteger(result.item.lastReviewedAt)||!/^[a-f0-9]{64}$/.test(result.item.stateHash||''))throw new AMCV2Error('Review acknowledgment is uncertain; retry this exact review or refresh to inspect the rule.',{code:'invalid_response'});
      return result;
    }
    async retireStandingOrder(id,expectedHash){
      if(typeof id!=='string'||!id||id.length>512||typeof expectedHash!=='string'||!/^[a-f0-9]{64}$/.test(expectedHash))throw new AMCV2Error('Retirement requires the observed rule state.',{code:'invalid_directive'});
      const result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({op:'retire',id,expectedHash})});
      if(result?.ok!==true||result.id!==id||typeof result.removed!=='boolean'||!Array.isArray(result.results)||result.results.length>40||!Array.isArray(result.needsCommit)||result.needsCommit.length>40)throw new AMCV2Error('Retirement outcome is uncertain. Refresh the rule and inspect its files before another action.',{code:'invalid_response'});
      return result;
    }
    async applyStandingOrder(id,expectedHash,target){
      if(typeof id!=='string'||!id||id.length>512||typeof expectedHash!=='string'||!/^[a-f0-9]{64}$/.test(expectedHash)||typeof target!=='string'||!target||target.length>8192)throw new AMCV2Error('Applying a rule requires its observed state and one selected target.',{code:'invalid_directive'});
      const result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({op:'plant-existing',id,expectedHash,targets:[target]})});
      if(result?.ok!==true||result.id!==id||result.item?.id!==id||!Array.isArray(result.results)||result.results.length!==1)throw new AMCV2Error('Application outcome is uncertain. Refresh the rule and inspect its target before another attempt.',{code:'invalid_response'});
      return result;
    }
    async standingOrderGitStatus(id,path){
      if(typeof id!=='string'||!id||id.length>512||typeof path!=='string'||!path||path.length>8192)throw new AMCV2Error('A registered rule and target path are required.',{code:'invalid_directive'});
      const result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({op:'git-status',id,path})});
      if(result?.ok!==true||!Array.isArray(result.states)||result.states.length!==1||result.states[0].path!==path)throw new AMCV2Error('Invalid repository status response.',{code:'invalid_response'});
      return result.states[0];
    }
    async standingOrderGitAction(op,id,path,expectedHash){
      if(!['git-commit','git-push'].includes(op)||typeof id!=='string'||!id||id.length>512||typeof path!=='string'||!path||path.length>8192||typeof expectedHash!=='string'||!/^[a-f0-9]{64}$/.test(expectedHash))throw new AMCV2Error('Git actions require one registered path and the observed rule state.',{code:'invalid_directive'});
      if(!this.csrf)throw new AMCV2Error('Desktop authentication is required before a Git action.',{code:'missing_csrf'});
      let result;
      try{result=await this._request('/api/directives',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({op,id,path,expectedHash})});}
      catch(error){throw new AMCV2Error('Git outcome may be uncertain. Inspect repository status and history before another action. No automatic retry was made. '+error.message,{code:error.code,status:error.status});}
      if(result?.ok!==true||typeof result.done!=='boolean'||typeof result.note!=='string'||result.uncertain!==undefined&&typeof result.uncertain!=='boolean'||result.done&&result.uncertain)throw new AMCV2Error('Git acknowledgment is invalid. Inspect repository status and history; do not blindly retry.',{code:'invalid_response'});
      return result;
    }
    async playbook(id) {
      if(typeof id!=='string'||!id||id.length>512)throw new AMCV2Error('A playbook identity is required.',{code:'invalid_playbook'});
      const result=await this._read('/api/playbooks?id='+encodeURIComponent(id));
      if(result?.item?.id!==id)throw new AMCV2Error('Invalid playbook response.',{code:'invalid_response'});
      return result.item;
    }
    async mutatePlaybook(operation) {
      const o=operation;
      if(!o||!['save','delete'].includes(o.op)||typeof o.operationId!=='string'||!/^[A-Za-z0-9_-]{1,128}$/.test(o.operationId)||!Number.isSafeInteger(o.expectedRevision)||o.expectedRevision<0||typeof o.id!=='string'||o.id.length>512||o.op==='delete'&&!o.id||!o.id&&o.expectedRevision!==0)throw new AMCV2Error('Invalid revision-bound Playbook operation.',{code:'invalid_playbook'});
      if(o.op==='save'&&(typeof o.name!=='string'||typeof o.body!=='string'||new TextEncoder().encode(o.body).byteLength>60000))throw new AMCV2Error('Playbook requires a name and at most 60,000 UTF-8 body bytes.',{code:'invalid_playbook'});
      if(!this.csrf)throw new AMCV2Error('Desktop authentication must be loaded before saving.',{code:'missing_csrf'});
      const body=JSON.stringify(o);
      if(new TextEncoder().encode(body).byteLength>1024*1024)throw new AMCV2Error('Playbook request is too large.',{code:'invalid_playbook'});
      const journalKey=this.key+':playbook-op:'+o.operationId;
      try {
        const existing=this.storage?.getItem(journalKey);
        if(existing&&existing!==body)throw new Error('Operation identity collision');
        const pending=this.pendingPlaybookOperations();
        if(!existing&&pending.length>=20)throw new Error('Pending operation limit reached');
        this.storage.setItem(journalKey,body);
      }catch{throw new AMCV2Error('The Playbook operation could not be saved locally. No request was sent.',{code:'blocked_local_storage'});}
      let result;
      try {result=await this._request('/api/playbooks',{method:'POST',headers:{'Content-Type':'application/json'},body});}
      catch(error){
        if(error.status>=400&&error.status<500&&![408,429].includes(error.status)){
          try{if(this.storage.getItem(journalKey)===body)this.storage.removeItem(journalKey);}catch{/* Preserve rather than erase an unconfirmed journal change. */}
        }
        throw error;
      }
      if(result?.ok!==true||result.operationId!==o.operationId||typeof result.id!=='string'||!result.id||o.id&&result.id!==o.id||o.op==='save'&&(result.item?.id!==result.id||result.item.revision!==o.expectedRevision+1)||o.op==='delete'&&result.deleted!==true)throw new AMCV2Error('Playbook acknowledgement is uncertain. Retry the same operation identity.',{code:'invalid_response',retryable:true});
      try{if(this.storage.getItem(journalKey)===body)this.storage.removeItem(journalKey);}catch{throw new AMCV2Error('Playbook was acknowledged, but the local operation could not be cleared. Retrying the same operation is safe.',{code:'blocked_local_storage'});}
      return result;
    }
    pendingPlaybookOperations() {
      const storage=this.storage,prefix=this.key+':playbook-op:',operations=[];
      if(!storage||typeof storage.length!=='number'||typeof storage.key!=='function')throw new AMCV2Error('Playbook recovery storage is unavailable.',{code:'blocked_local_storage'});
      for(let i=0;i<storage.length;i++){
        const key=storage.key(i);if(typeof key!=='string'||!key.startsWith(prefix))continue;
        const raw=storage.getItem(key);
        if(typeof raw!=='string'||raw.length>1024*1024||operations.length>=20)throw new AMCV2Error('Playbook recovery journal exceeds its bounds. Preserve it before recovery.',{code:'blocked_local_storage'});
        let o;try{o=JSON.parse(raw);}catch{throw new AMCV2Error('Playbook recovery journal is unreadable. Preserve it before recovery.',{code:'blocked_local_storage'});}
        if(!o||key!==prefix+o.operationId||!['save','delete'].includes(o.op)||typeof o.id!=='string'||!Number.isSafeInteger(o.expectedRevision)||o.expectedRevision<0||o.op==='save'&&(typeof o.name!=='string'||typeof o.body!=='string'))throw new AMCV2Error('Invalid Playbook recovery operation. Preserve it before recovery.',{code:'blocked_local_storage'});
        operations.push(o);
      }
      return operations;
    }
    async brainSnapshot(id, stamp) {
      if (typeof stamp !== 'string' || !stamp || stamp.length > 256) throw new AMCV2Error('A snapshot identity is required.', { code: 'invalid_brain_file' });
      return this._read('/api/brain/snapshot?id=' + this._brainID(id) + '&stamp=' + encodeURIComponent(stamp));
    }
    async saveBrainFile(id, content, expectedHash) {
      this._brainID(id);
      if (typeof content !== 'string' || new TextEncoder().encode(content).byteLength > 512 * 1024 || !/^[a-f0-9]{64}$/.test(expectedHash || '')) throw new AMCV2Error('A save requires the loaded file hash and at most 512 KiB of UTF-8 content.', { code: 'invalid_brain_file' });
      if (!this.csrf) throw new AMCV2Error('Load the desktop guidance inventory before saving.', { code: 'missing_csrf' });
      const body = JSON.stringify({ id, content, expectedHash });
      if (new TextEncoder().encode(body).byteLength > 1024 * 1024) throw new AMCV2Error('The encoded save exceeds the desktop request limit.', { code: 'invalid_brain_file' });
      // File writes have snapshots and hash conflicts, not the organization journal's
      // idempotency protocol. Never silently retry a possibly committed write.
      let result;
      try {
        result = await this._request('/api/brain/file', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body });
        if (result?.ok !== true || !/^[a-f0-9]{64}$/.test(result.expectedHash || '')) throw new AMCV2Error('Invalid save acknowledgement.', { code: 'invalid_response' });
      } catch (error) {
        if (error.status === 409) throw new AMCV2Error('The file changed on disk. Keep your draft and compare it with a fresh read before saving.', { code: 'brain_conflict', status: 409 });
        if (!error.status || error.status >= 500) throw new AMCV2Error('The save outcome is unknown. Keep your draft and reload the file and snapshot history before deciding whether to save again.', { code: 'brain_save_unknown', status: error.status || 0 });
        throw error;
      }
      return { ...result, id };
    }
    async session(id) { const epoch = this.recoveryEpoch; const row = await this._request('/api/v2/sessions/' + encodeURIComponent(id)); return this._acceptSession(row, epoch); }
    async rateCatalogs(after = '') { return this._read('/api/v2/rate-catalogs?after='+encodeURIComponent(after)); }
    async rateCatalog(id) { return this._read('/api/v2/rate-catalogs/'+encodeURIComponent(id)); }
    async importRateCatalog(text) {
      if(typeof text!=='string'||new TextEncoder().encode(text).byteLength>65536)throw new AMCV2Error('Catalog JSON must be at most 64 KiB.',{code:'invalid_catalog'});
      let catalog;
      try{catalog=JSON.parse(text);}catch{throw new AMCV2Error('Catalog must contain valid JSON.',{code:'invalid_catalog'});}
      if(!catalog?.id||!Array.isArray(catalog.rates)||!catalog.rates.length)throw new AMCV2Error('Catalog requires an ID and at least one rate.',{code:'invalid_catalog'});
      const epoch=this.recoveryEpoch;
      let result;
      try{result=await this._request('/api/v2/rate-catalogs',{method:'POST',headers:{'Content-Type':'application/json'},body:text});}
      catch(error){if(error.status===409)throw new AMCV2Error('That catalog ID already exists with different rates. Use a new ID; historical catalogs cannot be overwritten.',{code:'catalog_conflict',status:409});throw error;}
      if(epoch!==this.recoveryEpoch)throw stale();return result;
    }
    async pricingPolicy(id) {
      try { return await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/pricing-policy'); }
      catch(error) { if(error.status===404)return null;throw error; }
    }
    async pricingHistory(id, after='') {
      if(typeof after!=='string'||after.length>1024)throw new AMCV2Error('Invalid estimate-history cursor.',{code:'invalid_cursor'});
      const rows=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/pricing?after='+encodeURIComponent(after));
      if(!Array.isArray(rows)||rows.length>100)throw new AMCV2Error('Invalid estimate-history page.',{code:'invalid_response'});
      let prior=after;
      for(const row of rows){
        const p=row?.estimate;
        if(typeof row?.id!=='string'||row.id<=prior||row.sessionId!==id||typeof row.catalogId!=='string'||typeof row.context!=='string'||(row.comparisonAt!=null&&!Number.isFinite(Date.parse(row.comparisonAt)))||!p||!Number.isFinite(p.cost)||p.cost<0||![p.recordedTokens,p.pricedTokens,p.unpricedTokens,p.unattributedTokens].every(n=>Number.isSafeInteger(n)&&n>=0)||p.pricedTokens+p.unpricedTokens!==p.recordedTokens||p.unattributedTokens>p.recordedTokens)throw new AMCV2Error('Mismatched or malformed estimate history.',{code:'invalid_response'});
        prior=row.id;
      }
      return {rows,next:rows.length===100?prior:''};
    }
    async comparePricing(id, {catalogId, context, comparisonAt}) {
      if(!catalogId||!context||typeof comparisonAt!=='string'||!/(Z|[+-]\d\d:\d\d)$/.test(comparisonAt)||!Number.isFinite(Date.parse(comparisonAt)))throw new AMCV2Error('Choose a catalog, billing context, and comparison timestamp with a timezone.',{code:'invalid_comparison'});
      const epoch=this.recoveryEpoch;
      const result=await this._request('/api/v2/sessions/'+encodeURIComponent(id)+'/pricing',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({catalogId,context,comparisonAt})});
      if(epoch!==this.recoveryEpoch)throw stale();
      if(result?.sessionId!==id||result.catalogId!==catalogId||result.context!==context||Date.parse(result.comparisonAt)!==Date.parse(comparisonAt)||!result.estimate||!Number.isFinite(result.estimate.cost)||result.estimate.cost<0)throw new AMCV2Error('The hub returned a mismatched pricing comparison.',{code:'invalid_response'});
      return result;
    }
    async setPricingPolicy(id, policy) {
      if(policy&&(!policy.catalogId||!policy.context))throw new AMCV2Error('Choose a catalog and billing context.',{code:'invalid_pricing_policy'});
      if(policy?.comparisonAt!=null&&(typeof policy.comparisonAt!=='string'||!/(Z|[+-]\d\d:\d\d)$/.test(policy.comparisonAt)||!Number.isFinite(Date.parse(policy.comparisonAt))))throw new AMCV2Error('Choose a comparison timestamp with a timezone.',{code:'invalid_comparison'});
      const epoch=this.recoveryEpoch;
      const result=await this._request('/api/v2/sessions/'+encodeURIComponent(id)+'/pricing-policy',{
        method:policy?'PUT':'DELETE',headers:{'Content-Type':'application/json'},...(policy?{body:JSON.stringify(policy)}:{})
      });
      if(epoch!==this.recoveryEpoch)throw stale();
      return result;
    }
    async pricingDefault() {
      try {
        const p=await this._read('/api/v2/pricing-default');
        if(!p?.catalogId||!p.context||typeof p.comparisonAt!=='string'||!Number.isFinite(Date.parse(p.comparisonAt)))throw new AMCV2Error('Invalid pricing default returned by hub.',{code:'invalid_response'});
        return p;
      }catch(error){if(error.status===404)return null;throw error;}
    }
    async setPricingDefault(policy) {
      if(policy&&(!policy.catalogId||!policy.context||typeof policy.comparisonAt!=='string'||!/(Z|[+-]\d\d:\d\d)$/.test(policy.comparisonAt)||!Number.isFinite(Date.parse(policy.comparisonAt))))throw new AMCV2Error('Choose a catalog, billing context, and comparison timestamp with a timezone.',{code:'invalid_comparison'});
      const epoch=this.recoveryEpoch;
      const result=await this._request('/api/v2/pricing-default',{method:policy?'PUT':'DELETE',headers:{'Content-Type':'application/json'},...(policy?{body:JSON.stringify(policy)}:{})});
      if(epoch!==this.recoveryEpoch)throw stale();
      return result;
    }
    async sessions(options = {}) {
      const query = this._filters(options); const queryKey = String(query); const ticket = ++this.serial;
      this.queries.set(queryKey, ticket); const epoch = this.recoveryEpoch;
      try {
        const page = await this._request('/api/v2/sessions?' + query, {signal:options.signal});
        if (this.queries.get(queryKey) !== ticket || epoch !== this.recoveryEpoch) throw stale();
        if (!Array.isArray(page?.sessions) || page.sessions.length > positiveLimit(options.limit)) throw new AMCV2Error('The hub did not return a bounded session page.', { code: 'invalid_response' });
        return { sessions: page.sessions.map(row => this._acceptSession(row, epoch)), nextCursor: page.nextCursor || null };
      } finally { if (this.queries.get(queryKey) === ticket) this.queries.delete(queryKey); }
    }
    async _read(path, options = {}) { const epoch = this.recoveryEpoch; const value = await this._request(path, options); if (epoch !== this.recoveryEpoch) throw stale(); return value; }
    async totals(options = {}) { return this._read('/api/v2/totals?' + this._filters(options, false), {signal:options.signal}); }
    async contributionTotals(options = {}) {
      const result=await this._read('/api/v2/contribution-totals?'+this._filters(options,false),{signal:options.signal});
      const invalid=()=>{throw new AMCV2Error('The hub returned invalid contribution totals.',{code:'invalid_response'});};
      const keys=['tokensIn','tokensCache','tokensCacheWrite','tokensOut'];
      if(!['verified-duplicate-exclusions-only','verified-usage-exclusions-only'].includes(result?.scope)||!['sessions','activeExclusions','staleSelections'].every(k=>Number.isSafeInteger(result[k])&&result[k]>=0))invalid();
      for(const kind of ['recorded','excluded','counted']){
        const buckets=result[kind];
        if(!buckets||![...keys,'total'].every(k=>Number.isSafeInteger(buckets[k])&&buckets[k]>=0)||keys.reduce((n,k)=>n+buckets[k],0)!==buckets.total)invalid();
      }
      for(const k of [...keys,'total'])if(result.recorded[k]-result.excluded[k]!==result.counted[k])invalid();
      return result;
    }
    async analyticsTotals(options = {}) { return this._read('/api/v2/analytics/totals?' + this._filters(options, false)); }
    _economicsCaptureRecord(raw){
      if(typeof raw==='string'&&/^[A-Za-z0-9_-]{1,128}$/.test(raw))return {id:raw,recoveryEpoch:''};
      const record=JSON.parse(raw);
      if(!record||record.version!==1||typeof record.id!=='string'||!/^[A-Za-z0-9_-]{1,128}$/.test(record.id)||typeof record.recoveryEpoch!=='string'||record.recoveryEpoch.length>128)throw new Error('Invalid operation record');
      return record;
    }
    pendingEconomicsCapture(){
      try{
        if(!this.storage||typeof this.storage.length!=='number'||typeof this.storage.key!=='function')throw new Error('Storage unavailable');
        const base=this.key+':economics-capture',ids=new Set();
        const old=this.storage.getItem(base);
        if(old!==null){if(!/^[A-Za-z0-9_-]{1,128}$/.test(old))throw new Error('Invalid identity');ids.add(old);}
        for(let i=0;i<this.storage.length;i++){
          const key=this.storage.key(i);if(typeof key!=='string'||!key.startsWith(base+':'))continue;
          const {id}=this._economicsCaptureRecord(this.storage.getItem(key));
          if(key!==base+':'+id)throw new Error('Invalid operation record');
          ids.add(id);if(ids.size>100)throw new Error('Recovery bound exceeded');
        }
        return this.economicsCaptureRetryID||old||[...ids].sort()[0]||null;
      }catch{throw new AMCV2Error('Snapshot recovery storage is unavailable or damaged; preserve it before retrying.',{code:'blocked_local_storage'});}
    }
    captureEconomics(){
      if(this.economicsResolutionFlight)return Promise.reject(new AMCV2Error('Wait for capture reconciliation to finish.',{code:'capture_busy'}));
      if(this.economicsCaptureFlight)return this.economicsCaptureFlight;
      const work=this._captureEconomics();this.economicsCaptureFlight=work;
      work.finally(()=>{if(this.economicsCaptureFlight===work)this.economicsCaptureFlight=null;}).catch(()=>{});
      return work;
    }
    async _captureEconomics(){
      if(!this.csrf)throw new AMCV2Error('Desktop authentication is required before capturing history.',{code:'missing_csrf'});
      const base=this.key+':economics-capture',pending=this.pendingEconomicsCapture(),id=pending||this.uuid(),key=base+':'+id;
      if(!/^[A-Za-z0-9_-]{1,128}$/.test(id))throw new AMCV2Error('Invalid capture identity.',{code:'invalid_operation'});
      let record,raw;
      try{
        const prior=this.storage.getItem(key);
        record=this.economicsCaptureRetryRecord?.id===id?this.economicsCaptureRetryRecord:prior!==null?this._economicsCaptureRecord(prior):{id,recoveryEpoch:pending?'':this.recoveryEpoch||''};
        if(!pending&&!record.recoveryEpoch)throw new Error('Server identity unavailable');
        raw=JSON.stringify({version:1,id,recoveryEpoch:record.recoveryEpoch});
        this.storage.setItem(key,raw);if(this.storage.getItem(key)!==raw)throw new Error('Journal changed');
      }
      catch{throw new AMCV2Error('Could not preserve snapshot retry identity; nothing was sent.',{code:'blocked_local_storage'});}
      this.economicsCaptureRetryID=id;
      this.economicsCaptureRetryRecord=record;
      const requestEpoch=this.recoveryEpoch;
      let result;
      try{result=await this._request('/api/v2/analytics/economics/history',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({operationId:id,recoveryEpoch:record.recoveryEpoch})});}
      catch(error){
        if(error.code==='history_changed')throw new AMCV2Error('This pending snapshot is absent from the restored history, or its original server identity is unknown. Its retry record is preserved; a new measurement cannot replace it.',{code:'capture_reconciliation_required',status:409});
        throw error;
      }
      if(result?.id!==id||result.reason!=='manual'||result.measurement?.version!==1)throw new AMCV2Error('Capture acknowledgement is inconsistent; retry the pending operation.',{code:'invalid_response'});
      if(requestEpoch!==this.recoveryEpoch)throw new AMCV2Error('The hub was restored while this capture was pending. Retry to check whether its original snapshot survived.',{code:'capture_reconciliation_required',status:409});
      try{if(this.storage.getItem(key)===raw)this.storage.removeItem(key);if(this.storage.getItem(base)===id)this.storage.removeItem(base);}
      catch{throw new AMCV2Error('Capture succeeded but retry storage could not be cleared. Retrying will return the same snapshot.',{code:'blocked_local_storage'});}
      if(this.economicsCaptureRetryID===id)this.economicsCaptureRetryID=null;
      if(this.economicsCaptureRetryRecord?.id===id)this.economicsCaptureRetryRecord=null;
      return result;
    }
    resolvePendingEconomicsCapture(expectedID){
      if(this.economicsResolutionFlight)return expectedID===this.economicsResolutionID?this.economicsResolutionFlight:Promise.reject(new AMCV2Error('Another capture is being reconciled. Wait and review again.',{code:'capture_busy'}));
      this.economicsResolutionID=expectedID;
      const work=this._resolvePendingEconomicsCapture(expectedID);this.economicsResolutionFlight=work;
      work.finally(()=>{if(this.economicsResolutionFlight===work)this.economicsResolutionFlight=null;}).catch(()=>{});
      return work;
    }
    async economicsCaptureResolutions(options={}){
      if(Object.keys(options).some(key=>!['cursor','limit'].includes(key)))throw new AMCV2Error('Capture audit records cannot use current session filters.',{code:'invalid_filter'});
      const page=await this._cursorPage('/api/v2/analytics/economics/capture-resolutions','items',options);
      if(page.items.some(r=>!r||typeof r.id!=='string'||!/^[A-Za-z0-9_-]{1,128}$/.test(r.id)||r.outcome!=='capture-unavailable'||typeof r.originalEpoch!=='string'||r.originalEpoch.length>128||typeof r.resolvedEpoch!=='string'||!r.resolvedEpoch||r.resolvedEpoch.length>128||typeof r.resolvedAt!=='string'||!Number.isFinite(Date.parse(r.resolvedAt))))throw new AMCV2Error('Capture audit page is inconsistent.',{code:'invalid_response'});
      return page;
    }
    async _resolvePendingEconomicsCapture(expectedID){
      if(this.economicsCaptureFlight)throw new AMCV2Error('Wait for the pending capture request before resolving it.',{code:'capture_busy'});
      if(!this.csrf||!this.recoveryEpoch)throw new AMCV2Error('Refresh the desktop connection before resolving a capture.',{code:'missing_server_identity'});
      const id=this.pendingEconomicsCapture();
      if(!id)throw new AMCV2Error('There is no pending capture to resolve.',{code:'no_pending_capture'});
      if(id!==expectedID)throw new AMCV2Error('The pending capture changed. Review it again before confirming.',{code:'capture_conflict'});
      const base=this.key+':economics-capture',key=base+':'+id;
      let record,raw;
      try{raw=this.storage.getItem(key);record=this.economicsCaptureRetryRecord?.id===id?this.economicsCaptureRetryRecord:raw!==null?this._economicsCaptureRecord(raw):{id,recoveryEpoch:''};}
      catch{throw new AMCV2Error('Recovery storage is damaged; preserve it before continuing.',{code:'blocked_local_storage'});}
      const requestEpoch=this.recoveryEpoch;
      const result=await this._request('/api/v2/analytics/economics/capture-resolutions',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({operationId:id,originalEpoch:record.recoveryEpoch,recoveryEpoch:requestEpoch})});
      if(result?.id!==id||result.originalEpoch!==record.recoveryEpoch||result.outcome!=='capture-unavailable'||typeof result.resolvedEpoch!=='string'||!result.resolvedEpoch||typeof result.resolvedAt!=='string'||!Number.isFinite(Date.parse(result.resolvedAt)))throw new AMCV2Error('Reconciliation acknowledgement is inconsistent; preserve the pending record.',{code:'invalid_response'});
      if(requestEpoch!==this.recoveryEpoch)throw new AMCV2Error('The hub was restored during reconciliation. Retry to verify that the audit record survived.',{code:'capture_reconciliation_required',status:409});
      try{if(this.storage.getItem(key)===raw)this.storage.removeItem(key);if(this.storage.getItem(base)===id)this.storage.removeItem(base);}
      catch{throw new AMCV2Error('The unavailable attempt was recorded, but local recovery storage could not be cleared. Retry this resolution.',{code:'blocked_local_storage'});}
      if(this.economicsCaptureRetryID===id)this.economicsCaptureRetryID=null;
      if(this.economicsCaptureRetryRecord?.id===id)this.economicsCaptureRetryRecord=null;
      return result;
    }
    async legacyEconomics(options={}){
      if(Object.keys(options).some(key=>!['cursor','limit'].includes(key)))throw new AMCV2Error('Legacy fleet evidence cannot use current session filters.',{code:'invalid_filter'});
      const page=await this._cursorPage('/api/v2/analytics/economics/legacy','items',options);
      if(page.evidence!=='legacy-unverified'||page.items.some((item,i)=>!Number.isSafeInteger(item.offset)||item.offset<0||!Number.isSafeInteger(item.length)||item.length<1||item.length>65536||typeof item.complete!=='boolean'||typeof item.validObject!=='boolean'||typeof item.raw!=='string'||item.raw.length>65536||i>0&&item.offset!==page.items[i-1].offset+page.items[i-1].length)||page.items.reduce((n,item)=>n+item.length,0)>(1<<20)+65536)throw new AMCV2Error('Legacy economics page is inconsistent or exceeds its display bound. Preserve the raw download.',{code:'invalid_response'});
      return page;
    }
    async economicsHistory(options={}){
      if(Object.keys(options).some(key=>!['cursor','limit'].includes(key)))throw new AMCV2Error('Historical fleet measurements cannot use present-day session filters.',{code:'invalid_filter'});
      const page=await this._cursorPage('/api/v2/analytics/economics/history','items',options);
      const count=value=>Number.isSafeInteger(value)&&value>=0;
      if(page.items.some(item=>{
        const m=item?.measurement,c=m?.costs;
        return typeof item?.id!=='string'||!['timer','manual'].includes(item.reason)||m?.version!==1||typeof m.measuredAt!=='string'||!Number.isFinite(Date.parse(m.measuredAt))||!c||!['sessions','recordedTokens','pricedTokens','unpricedOrUnmeasuredTokens'].every(key=>count(c[key]))||c.pricedTokens+c.unpricedOrUnmeasuredTokens!==c.recordedTokens||c.knownCost!==null&&(!Number.isFinite(c.knownCost)||c.knownCost<0);
      }))throw new AMCV2Error('Historical measurement evidence is inconsistent or unsupported.',{code:'invalid_response'});
      return page;
    }
    async economicsCosts(options={}){
      const result=await this._read('/api/v2/analytics/economics/costs?'+this._filters(options,false));
      const counts=['sessions','recordedTokens','pricedTokens','unpricedOrUnmeasuredTokens','pricedTokensWithoutBreakdown','sessionsWithBreakdown','sessionsWithoutBreakdown'];
      if(!result||counts.some(key=>!Number.isSafeInteger(result[key])||result[key]<0)||!result.components||!Number.isSafeInteger(result.components.pricedTokens)||result.components.pricedTokens<0||['input','cacheRead','cacheWrite','output'].some(key=>!Number.isFinite(result.components[key])||result.components[key]<0)||result.knownCost!==null&&(!Number.isFinite(result.knownCost)||result.knownCost<0)||result.pricedTokens+result.unpricedOrUnmeasuredTokens!==result.recordedTokens||result.components.pricedTokens+result.pricedTokensWithoutBreakdown!==result.pricedTokens||result.sessionsWithBreakdown+result.sessionsWithoutBreakdown!==result.sessions)throw new AMCV2Error('Economics cost coverage is inconsistent or unavailable.',{code:'invalid_response'});
      return result;
    }
    async economicsLifetimes(options={}){
      const data=await this._read('/api/v2/analytics/economics/lifetimes?'+this._filters(options,false));
      const count=value=>Number.isSafeInteger(value)&&value>=0;
      const fields=['agents','messages','recordedTokens','costEligibleAgents','costEligibleMessages'];
      if(data?.scope!=='claude-child-usage-messages-v1'||!['childSources','withoutMessageObservations','withUnstableMessageIdentity'].every(key=>count(data[key]))||!Array.isArray(data.buckets)||data.buckets.length!==8||data.buckets.some(b=>!b||typeof b.label!=='string'||!fields.every(key=>count(b[key]))||b.costEligibleAgents>b.agents||b.costEligibleMessages>b.messages||b.comparableCost!==null&&(!Number.isFinite(b.comparableCost)||b.comparableCost<0)||b.comparableCost!==null&&b.costEligibleMessages===0)||data.withoutMessageObservations+data.buckets.reduce((sum,b)=>sum+b.agents,0)!==data.childSources||data.withUnstableMessageIdentity>data.childSources)throw new AMCV2Error('Lifetime accounting is inconsistent or unavailable.',{code:'invalid_response'});
      return data;
    }
    async _cursorPage(path, key, options = {}) {
      const page = await this._read(path + '?' + this._filters(options), {signal:options.signal});
      if (!Array.isArray(page?.[key]) || page[key].length > positiveLimit(options.limit) || page.nextCursor != null && typeof page.nextCursor !== 'string') throw new AMCV2Error('The hub did not return a bounded page.', { code: 'invalid_response' });
      if (page.nextCursor && page.nextCursor === options.cursor) throw new AMCV2Error('The hub repeated a cursor.', { code: 'cursor_not_advancing' });
      return { ...page, nextCursor: page.nextCursor || null };
    }
    async *_cursorPages(read, options = {}) { let cursor = options.cursor || null; do { const page = await read({ ...options, cursor }); yield page; cursor = page.nextCursor; } while (cursor); }
    async rings(options={}) {
      if(typeof options.timezone!=='string'||!options.timezone||options.timezone==='Local'||options.timezone.length>128||!options.project&&!options.unassignedProject||options.cursor||options.limit!=null)throw new AMCV2Error('Invalid Rings scope.',{code:'invalid_filter'});
      const query=this._filters(options,false);query.set('timezone',options.timezone);
      const value=await this._read('/api/v2/analytics/rings?'+query,{signal:options.signal}),count=n=>Number.isSafeInteger(n)&&n>=0;
      const counts=['sessions','sessionsWithErrors','sessionsWithIncompleteResults','recordedTokens','pricedTokens','tierPricedTokens'];
      if(!value||value.project!==(options.project||'')||value.timezone!==options.timezone||!['sessions','olderSessions','undatedSessions'].every(k=>count(value[k]))||!Array.isArray(value.weeks)||value.weeks.length>10||value.weeks.some((w,i)=>!w||w.index!==i||!/^\d{4}-\d{2}-\d{2}$/.test(w.week)||!Number.isFinite(Date.parse(w.from))||!Number.isFinite(Date.parse(w.to))||Date.parse(w.from)>=Date.parse(w.to)||i>0&&w.from!==value.weeks[i-1].to||counts.some(k=>!count(w[k]))||w.sessionsWithErrors>w.sessions||w.sessionsWithIncompleteResults>w.sessions||w.tierPricedTokens>w.pricedTokens||w.pricedTokens>w.recordedTokens||['costEstimate','classifiedCost','topTierCost'].some(k=>w[k]!==null&&(!Number.isFinite(w[k])||w[k]<0))||(w.tierPricedTokens===0)!==(w.classifiedCost===null)||(w.classifiedCost===null)!==(w.topTierCost===null)||w.classifiedCost!==null&&(w.costEstimate===null||w.topTierCost>w.classifiedCost+1e-9||w.classifiedCost>w.costEstimate+1e-9))||value.weeks.reduce((n,w)=>n+w.sessions,0)+value.olderSessions+value.undatedSessions!==value.sessions)throw new AMCV2Error('Invalid Rings evidence.',{code:'invalid_response'});
      return value;
    }
    async rhythm(options = {}) {
      if(typeof options.timezone!=='string'||!options.timezone||options.timezone==='Local'||options.timezone.length>128||options.cursor||options.limit!=null)throw new AMCV2Error('Invalid rhythm filters.',{code:'invalid_filter'});
      const query=this._filters(options,false);query.set('timezone',options.timezone);
      const result=await this._read('/api/v2/analytics/rhythm?'+query);
      const counts=['sessions','sessionsWithErrors','sessionsWithIncompleteResults','recordedTokens','pricedTokens','tierPricedTokens'];
      const badBucket=(b,i)=>!b||b.index!==i||counts.some(k=>!Number.isSafeInteger(b[k])||b[k]<0)||b.sessionsWithErrors>b.sessions||b.sessionsWithIncompleteResults>b.sessions||b.pricedTokens>b.recordedTokens||b.tierPricedTokens>b.pricedTokens||['costEstimate','classifiedCost','topTierCost'].some(k=>b[k]!==null&&(!Number.isFinite(b[k])||b[k]<0))||(b.tierPricedTokens===0)!==(b.classifiedCost===null)||(b.classifiedCost===null)!==(b.topTierCost===null)||b.classifiedCost!==null&&(b.costEstimate===null||b.classifiedCost>b.costEstimate+1e-9||b.topTierCost>b.classifiedCost+1e-9);
      if(!result||result.timezone!==options.timezone||typeof result.basis!=='string'||['sessions','undatedSessions','outsideRangeSessions'].some(k=>!Number.isSafeInteger(result[k])||result[k]<0)||!Array.isArray(result.hours)||result.hours.length!==24||!Array.isArray(result.weekdays)||result.weekdays.length!==7||result.hours.some(badBucket)||result.weekdays.some(badBucket))throw new AMCV2Error('Invalid rhythm evidence.',{code:'invalid_response'});
      for(const buckets of [result.hours,result.weekdays])if(buckets.reduce((n,b)=>n+b.sessions,0)!==result.sessions)throw new AMCV2Error('Rhythm population is inconsistent.',{code:'invalid_response'});
      for(const key of counts)if(result.hours.reduce((n,b)=>n+b[key],0)!==result.weekdays.reduce((n,b)=>n+b[key],0))throw new AMCV2Error('Rhythm aggregates disagree.',{code:'invalid_response'});
      return result;
    }
    async calendar(options = {}) {
      const query=this._filters(options);
      if(options.snapshot){if(!/^[a-f0-9]{64}$/.test(options.snapshot))throw new AMCV2Error('Invalid calendar snapshot.',{code:'invalid_filter'});query.set('snapshot',options.snapshot);}
      for(const key of ['start','end','timezone']) {
        if(typeof options[key]!=='string'||!options[key]||options[key].length>128)throw new AMCV2Error('Invalid calendar range.',{code:'invalid_filter'});
        query.set(key,options[key]);
      }
      const page=await this._read('/api/v2/analytics/calendar?'+query);
      if(!/^[a-f0-9]{64}$/.test(page?.snapshot||''))throw new AMCV2Error('Calendar revision is missing.',{code:'invalid_response'});
      if(options.snapshot&&page.snapshot!==options.snapshot)throw new AMCV2Error('Calendar changed; refresh from the first day.',{code:'history_changed'});
      const counts=['sessions','sessionsWithEstimate','recordedTokens','pricedTokens','tierPricedTokens','errors','unknownToolResults','agentScopes'];
      if(!Array.isArray(page?.days)||page.days.length>positiveLimit(options.limit)||page.timezone!==options.timezone||typeof page.basis!=='string'||page.nextCursor!=null&&typeof page.nextCursor!=='string'||page.days.some((d,i)=>!d||!/^\d{4}-\d{2}-\d{2}$/.test(d.day)||d.day<options.start||d.day>=options.end||i>0&&d.day<=page.days[i-1].day||!Number.isFinite(Date.parse(d.from))||!Number.isFinite(Date.parse(d.to))||Date.parse(d.from)>=Date.parse(d.to)||counts.some(k=>!Number.isSafeInteger(d[k])||d[k]<0)||d.sessionsWithEstimate>d.sessions||d.pricedTokens>d.recordedTokens||['costEstimate','topTierCost'].some(k=>d[k]!==null&&(!Number.isFinite(d[k])||d[k]<0))))throw new AMCV2Error('Invalid calendar evidence.',{code:'invalid_response'});
      if(page.nextCursor&&(!page.days.length||page.nextCursor===options.cursor))throw new AMCV2Error('Calendar cursor did not advance.',{code:'cursor_not_advancing'});
      if(page.days.some(d=>d.tierPricedTokens>d.pricedTokens||(d.tierPricedTokens===0)!==(d.topTierCost===null)||d.topTierCost!==null&&(d.costEstimate===null||d.topTierCost>d.costEstimate+1e-9)))throw new AMCV2Error('Calendar tier coverage is inconsistent.',{code:'invalid_response'});
      return {...page,nextCursor:page.nextCursor||null};
    }
    async projects(options = {}) {
      const page=await this._cursorPage('/api/v2/projects','items',options);
      if(page.items.some(p=>!this._validProject(p)||p.deleted))throw new AMCV2Error('Invalid project registry page.',{code:'invalid_response'});
      return page;
    }
    _validProject(p){return !!p&&typeof p.id==='string'&&!!p.id&&p.id.length<=1024&&typeof p.name==='string'&&p.name.length<=1024&&typeof p.color==='string'&&/^#[a-f0-9]{6}$/i.test(p.color)&&Number.isSafeInteger(p.revision)&&p.revision>0&&typeof p.deleted==='boolean'&&typeof p.updatedAt==='string'&&Number.isFinite(Date.parse(p.updatedAt));}
    _projectOperation(o){
      if(!o||typeof o.id!=='string'||!o.id||o.id.length>1024||o.id.includes('\0')||typeof o.operationId!=='string'||!/^[A-Za-z0-9_-]{1,128}$/.test(o.operationId)||!Number.isSafeInteger(o.revision)||o.revision<0||typeof o.delete!=='boolean'||typeof o.recoveryEpoch!=='string'||!o.recoveryEpoch||o.recoveryEpoch.length>128||typeof o.name!=='string'||new TextEncoder().encode(o.name).byteLength>1024||typeof o.color!=='string'||!o.delete&&(!o.name.trim()||!/^#[a-f0-9]{6}$/i.test(o.color)))throw new AMCV2Error('Invalid revision-bound project operation.',{code:'invalid_project'});
      return {id:o.id,name:o.name,color:o.color,revision:o.revision,delete:o.delete,operationId:o.operationId,recoveryEpoch:o.recoveryEpoch};
    }
    pendingProjectOperations(){
      const prefix=this.key+':project-op:',operations=[],storage=this.storage;
      if(!storage||typeof storage.length!=='number'||typeof storage.key!=='function')throw new AMCV2Error('Project recovery storage is unavailable.',{code:'blocked_local_storage'});
      for(let i=0;i<storage.length;i++){
        const key=storage.key(i);if(typeof key!=='string'||!key.startsWith(prefix))continue;
        try{
          const raw=storage.getItem(key);if(typeof raw!=='string'||raw.length>8192||operations.length>=20)throw new Error('Journal bounds exceeded');
          const o=this._projectOperation(JSON.parse(raw));if(key!==prefix+o.operationId)throw new Error('Identity mismatch');operations.push(o);
        }catch{throw new AMCV2Error('Project recovery journal is invalid. Preserve it before recovery.',{code:'blocked_local_storage'});}
      }
      return operations;
    }
    mutateProject(operation){
      let o;try{o=this._projectOperation(operation);}catch(e){return Promise.reject(e);}
      const body=JSON.stringify(o),flights=this.projectFlights||(this.projectFlights=new Map()),prior=flights.get(o.id);
      if(prior)return prior.body===body?prior.work:Promise.reject(new AMCV2Error('A project write is already pending.',{code:'project_busy'}));
      const work=this._mutateProject(o,body);flights.set(o.id,{body,work});
      work.finally(()=>{if(flights.get(o.id)?.work===work)flights.delete(o.id);}).catch(()=>{});return work;
    }
    async _mutateProject(o,body){
      if(!this.csrf)throw new AMCV2Error('Desktop authentication is required.',{code:'missing_csrf'});
      const key=this.key+':project-op:'+o.operationId;
      try{
        const pending=this.pendingProjectOperations(),existing=this.storage.getItem(key);
        if(existing&&existing!==body||!existing&&pending.length>=20||pending.some(p=>p.id===o.id&&p.operationId!==o.operationId))throw new Error('Unresolved project operation');
        this.storage.setItem(key,body);if(this.storage.getItem(key)!==body)throw new Error('Journal did not persist');
      }catch{throw new AMCV2Error('The project intent could not be preserved. No request was sent.',{code:'blocked_local_storage'});}
      const epoch=this.recoveryEpoch;let result;
      try{result=await this._request('/api/v2/projects',{method:'POST',headers:{'Content-Type':'application/json',...(o.delete?{Prefer:'respond-async'}:{})},body});}
      catch(error){
        if(error.code!=='history_changed'&&error.status>=400&&error.status<500&&![401,403,408,429].includes(error.status)){try{if(this.storage.getItem(key)===body)this.storage.removeItem(key);}catch{}}
        throw error;
      }
      if(o.delete&&result?.operationId!=null){
        let processed=-1;
        for(;;){
          if(result.operationId!==o.operationId||!['pending','complete'].includes(result.state)||!Number.isSafeInteger(result.processed)||result.processed<0||result.processed<processed||!this._validProject(result.project)||result.project.id!==o.id||result.project.revision!==o.revision+1||!result.project.deleted||epoch!==this.recoveryEpoch)throw new AMCV2Error('Deletion progress could not be verified. Retry the saved operation.',{code:'invalid_response',retryable:true});
          processed=result.processed;
          if(result.state==='complete'){result=result.project;break;}
          if(result.problem)throw new AMCV2Error('Project deletion is blocked at the hub and will retry. Your saved intent is retained.',{code:'project_deletion_blocked',retryable:true});
          this._status('project_deletion_pending',{projectId:o.id,processed});
          await new Promise(resolve=>setTimeout(resolve,1000));
          result=await this._read('/api/v2/project-deletions/'+encodeURIComponent(o.operationId));
        }
      }
      if(!this._validProject(result)||result.id!==o.id||result.revision!==o.revision+1||result.deleted!==o.delete||!o.delete&&(result.name!==o.name||result.color!==o.color)||epoch!==this.recoveryEpoch)throw new AMCV2Error('Project outcome is uncertain. Retry the same saved operation.',{code:'invalid_response',retryable:true});
      try{if(this.storage.getItem(key)===body)this.storage.removeItem(key);}catch{throw new AMCV2Error('Project acknowledged; retry storage could not be cleared.',{code:'blocked_local_storage'});}
      return result;
    }
    async projectHistory(id,{after=0,limit=100}={}){
      if(typeof id!=='string'||!id||id.length>1024)throw new AMCV2Error('A project identity is required.',{code:'invalid_project'});
      const page=await this._read('/api/v2/projects/'+encodeURIComponent(id)+'/history?after='+sequence(after)+'&limit='+positiveLimit(limit));
      if(!Array.isArray(page?.items)||page.items.length>positiveLimit(limit)||page.items.some(a=>!Number.isSafeInteger(a.revision)||a.revision<=after||!this._validProject(a.after)||a.after.id!==id))throw new AMCV2Error('Invalid project audit response.',{code:'invalid_response'});
      let previous=sequence(after);for(const a of page.items){if(a.revision<=previous||a.after.revision!==a.revision)throw new AMCV2Error('Project audit did not advance.',{code:'invalid_response'});previous=a.revision;}
      if(page.next!=null&&(!Number.isSafeInteger(page.next)||page.next!==0&&(!page.items.length||page.next!==previous)))throw new AMCV2Error('Invalid project audit cursor.',{code:'invalid_response'});
      return page;
    }
    async projectCatalog(options = {}) { return this._cursorPage('/api/v2/catalog/projects', 'items', options); }
    async rankedProjectCatalog(options = {}) { return this._cursorPage('/api/v2/catalog/projects-ranked', 'items', options); }
    async machineCatalog(options = {}) { return this._cursorPage('/api/v2/catalog/machines', 'items', options); }
    projectPages(options = {}) { return this._cursorPages(page => this.projectCatalog(page), options); }
    machineCatalogPages(options = {}) { return this._cursorPages(page => this.machineCatalog(page), options); }
    async usageGroups(dimension, options = {}) {
      if (!['project', 'machine', 'provider', 'model', 'day', 'week', 'month', 'year', 'agentKind'].includes(dimension)) throw new AMCV2Error('Unsupported usage grouping.', { code: 'invalid_filter' });
      return this._cursorPage('/api/v2/analytics/usage/' + dimension, 'groups', options);
    }
    usageGroupPages(dimension, options = {}) { return this._cursorPages(page => this.usageGroups(dimension, page), options); }
    async localIdentity({signal}={}){
      const value=await this._read('/api/v2/local/identity',{signal});
      if(!value||typeof value.machineId!=='string'||!['configured','unconfigured'].includes(value.state)||(value.state==='configured')!==!!value.machineId)throw new AMCV2Error('Invalid local machine binding.',{code:'invalid_response'});
      return value;
    }
    async unsavedCandidates(machineId,{cursor='',limit=20,signal}={}){
      if(typeof machineId!=='string'||!machineId||new TextEncoder().encode(machineId).byteLength>1024)throw new AMCV2Error('An explicit local machine identity is required.',{code:'invalid_filter'});
      limit=Math.min(100,positiveLimit(limit));
      const page=await this._read('/api/v2/unsaved-candidates?'+new URLSearchParams({machineId,cursor,limit:String(limit)}),{signal});
      const count=n=>Number.isSafeInteger(n)&&n>=0;
      if(page?.machineId!==machineId||!Array.isArray(page.files)||page.files.length>limit||!count(page.totalFiles)||page.totalFiles<page.files.length||!count(page.totalSessions)||!count(page.indexedSessions)||page.indexedSessions>page.totalSessions||!/^[a-f0-9]{64}$/.test(page.snapshot||'')||page.nextCursor!=null&&typeof page.nextCursor!=='string'||page.nextCursor&&page.nextCursor===cursor)throw new AMCV2Error('Invalid local transcript candidate page.',{code:'invalid_response'});
      const seen=new Set();
      for(const f of page.files){if(!f||typeof f!=='object')throw new AMCV2Error('Invalid candidate row.',{code:'invalid_response'});const key=JSON.stringify([f.machineId,f.project,f.path,f.workingDirectory||'']);if(f.workingDirectory!=null&&(typeof f.workingDirectory!=='string'||new TextEncoder().encode(f.workingDirectory).byteLength>4096||/[\x00-\x1f\x7f]/.test(f.workingDirectory))||f.machineId!==machineId||typeof f.project!=='string'||typeof f.path!=='string'||!f.path||typeof f.sessionId!=='string'||!f.sessionId||typeof f.lastTouched!=='string'||f.lastTouched&&!Number.isFinite(Date.parse(f.lastTouched))||!Number.isSafeInteger(f.sessions)||f.sessions<1||f.sessions>page.totalSessions||seen.has(key))throw new AMCV2Error('Candidate provenance did not match its machine or page.',{code:'invalid_response'});seen.add(key);}
      return page;
    }
    async behaviorRoles({cursor='',limit=20,archived=false,signal}={}){
      if(![true,false,null].includes(archived))throw new AMCV2Error('Invalid archive filter.',{code:'invalid_filter'});
      limit=Math.min(100,positiveLimit(limit));const query=new URLSearchParams({cursor,limit:String(limit)});if(archived!==null)query.set('archived',String(archived));
      const page=await this._read('/api/v2/behavior-roles?'+query,{signal}),count=n=>Number.isSafeInteger(n)&&n>=0;
      if(!Array.isArray(page?.roles)||page.roles.length>limit||!count(page.totalRoles)||page.totalRoles<page.roles.length||!count(page.totalAppearances)||page.totalRoles>page.totalAppearances||!/^[a-f0-9]{64}$/.test(page.snapshot||'')||page.nextCursor!=null&&typeof page.nextCursor!=='string'||page.nextCursor&&page.nextCursor===cursor)throw new AMCV2Error('Invalid role-summary page.',{code:'invalid_response'});
      const seen=new Set(),counts=['appearances','sessions','machines','errors','withErrors','withoutReportedErrors','unknown','durationObserved','recordedTokens','pricedTokens','unpricedTokens','unattributedTokens','withEstimate'];
      for(const r of page.roles){if(!r||typeof r.name!=='string'||!r.name||r.name.length>120||seen.has(r.name)||counts.some(k=>!count(r[k]))||r.appearances<1||r.appearances>page.totalAppearances||r.sessions<1||r.sessions>r.appearances||r.machines<1||r.machines>r.sessions||r.withErrors+r.withoutReportedErrors+r.unknown!==r.appearances||r.errors<r.withErrors||r.durationObserved>r.appearances||r.withEstimate>r.appearances||r.pricedTokens+r.unpricedTokens!==r.recordedTokens||r.unattributedTokens>r.recordedTokens||(r.meanObservedMs===null?r.durationObserved!==0:!Number.isFinite(r.meanObservedMs)||r.meanObservedMs<0||r.durationObserved===0)||(r.knownCost===null?r.withEstimate!==0:!Number.isFinite(r.knownCost)||r.knownCost<0||r.withEstimate===0)||typeof r.exampleSessionId!=='string'||!r.exampleSessionId||typeof r.lastActivity!=='string'||r.lastActivity&&!Number.isFinite(Date.parse(r.lastActivity)))throw new AMCV2Error('Invalid role counts, duration or pricing evidence.',{code:'invalid_response'});seen.add(r.name);}
      return page;
    }
    async behaviorPatterns({cursor='',limit=20,archived=false,signal}={}){
      if(![true,false,null].includes(archived))throw new AMCV2Error('Invalid archive filter.',{code:'invalid_filter'});
      limit=Math.min(100,positiveLimit(limit));const query=new URLSearchParams({cursor,limit:String(limit)});if(archived!==null)query.set('archived',String(archived));
      const page=await this._read('/api/v2/behavior-patterns?'+query,{signal}),count=n=>Number.isSafeInteger(n)&&n>=0;
      if(!Array.isArray(page?.patterns)||page.patterns.length>limit||!count(page.totalPatterns)||page.totalPatterns<page.patterns.length||!count(page.totalSessions)||!/^[a-f0-9]{64}$/.test(page.snapshot||'')||page.nextCursor!=null&&typeof page.nextCursor!=='string'||page.nextCursor&&page.nextCursor===cursor)throw new AMCV2Error('Invalid behavior-pattern page.',{code:'invalid_response'});
      const seen=new Set();
      for(const p of page.patterns){const key=JSON.stringify([p?.fanout,p?.tools,p?.outcome]);if(!p||!['solo','small-team','team','fleet','unobserved','incomplete-attribution'].includes(p.fanout)||!['reported-errors','unknown','no-indexed-events','no-reported-errors'].includes(p.outcome)||!Array.isArray(p.tools)||p.tools.length>3||p.tools.some(t=>typeof t!=='string'||t.length>4096)||!count(p.sessions)||p.sessions<1||p.sessions>page.totalSessions||typeof p.exampleSessionId!=='string'||!p.exampleSessionId||typeof p.lastActivity!=='string'||p.lastActivity&&!Number.isFinite(Date.parse(p.lastActivity))||seen.has(key))throw new AMCV2Error('Invalid behavior-pattern evidence.',{code:'invalid_response'});seen.add(key);}
      return page;
    }
    async checkLocalSecrets(machineId,files,{signal}={}){
      const valid=s=>typeof s==='string'&&new TextEncoder().encode(s).byteLength<=4096&&!/[\x00-\x1f\x7f]/.test(s);
      if(typeof machineId!=='string'||!machineId||!Array.isArray(files)||files.length<1||files.length>25||files.some(f=>!f||!valid(f.path)||!f.path||f.workingDirectory!=null&&!valid(f.workingDirectory)))throw new AMCV2Error('A bound machine and one to 25 recorded file contexts are required.',{code:'invalid_filter'});
      if(!this.csrf)throw new AMCV2Error('Reload the desktop before scanning local files.',{code:'missing_csrf'});
      const value=await this._read('/api/v2/local/secrets',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({machineId,files:files.map(f=>({path:f.path,workingDirectory:f.workingDirectory||''}))}),signal});
      const states=['scanned','unknown','unsupported-path','outside-approved-roots','missing','ignored','skipped-noise','too-large','binary'];
      if(value?.machineId!==machineId||value.scope!=='approved-local-paths-only'||!Number.isFinite(Date.parse(value.observedAt))||!Array.isArray(value.files)||value.files.length!==files.length||value.files.some((f,i)=>!f||f.requestedPath!==files[i].path||(f.requestedWorkingDirectory||'')!==(files[i].workingDirectory||'')||typeof f.path!=='string'||!states.includes(f.state)||typeof f.capped!=='boolean'||!Array.isArray(f.findings)||f.findings.length>5||f.state!=='scanned'&&f.findings.length||['unknown','unsupported-path'].includes(f.state)&&!f.problem||f.findings.some(h=>!h||typeof h.kind!=='string'||h.kind.length>80||!Number.isSafeInteger(h.line)||h.line<1||typeof h.fragment!=='string'||!/^.{4}…$/u.test(h.fragment))))throw new AMCV2Error('Secret-scan evidence did not match the requested machine and file contexts.',{code:'invalid_response'});
      return value;
    }
    async checkLocalUnsaved(machineId,paths,{signal}={}){
      const valid=s=>typeof s==='string'&&new TextEncoder().encode(s).byteLength<=4096&&!/[\x00-\x1f\x7f]/.test(s);
      if(typeof machineId!=='string'||!machineId||!Array.isArray(paths)||paths.length<1||paths.length>25)throw new AMCV2Error('A bound machine and one to 25 local paths are required.',{code:'invalid_filter'});
      const contextual=typeof paths[0]==='object',files=paths.map(p=>contextual?p:{path:p});
      if(paths.some(p=>contextual?(!p||typeof p!=='object'||Array.isArray(p)):typeof p!=='string')||files.some(f=>!valid(f.path)||!f.path||f.workingDirectory!=null&&!valid(f.workingDirectory)))throw new AMCV2Error('Invalid recorded file context.',{code:'invalid_filter'});
      if(!this.csrf)throw new AMCV2Error('Reload the desktop before checking local files.',{code:'missing_csrf'});
      const body=contextual?{machineId,files:files.map(f=>({path:f.path,workingDirectory:f.workingDirectory||''}))}:{machineId,paths};
      const value=await this._read('/api/v2/local/unsaved',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body),signal});
      const states=['unknown','unsupported-path','outside-approved-roots','missing','ignored','tracked','in-history','untracked-no-reachable-history'];
      if(value?.machineId!==machineId||value.scope!=='approved-local-paths-only'||!Number.isFinite(Date.parse(value.observedAt))||!Array.isArray(value.files)||value.files.length!==files.length||value.files.some((f,i)=>!f||f.requestedPath!==files[i].path||(f.requestedWorkingDirectory||'')!==(files[i].workingDirectory||'')||typeof f.path!=='string'||!f.path||!states.includes(f.state)||['unknown','unsupported-path'].includes(f.state)&&(typeof f.problem!=='string'||!f.problem)||f.modifiedAt!=null&&!Number.isFinite(Date.parse(f.modifiedAt))||f.sizeBytes!=null&&(!Number.isSafeInteger(f.sizeBytes)||f.sizeBytes<0)))throw new AMCV2Error('Local file evidence did not match the requested machine and paths.',{code:'invalid_response'});
      return value;
    }
    async troubleFiles({cursor='',limit=20,sort='sessions',direction='desc',signal}={}){
      if(!['path','sessions','badSessions','unknownSessions','rate','lastTouched'].includes(sort)||!['asc','desc'].includes(direction))throw new AMCV2Error('Invalid Trouble Files sort.',{code:'invalid_filter'});
      limit=Math.min(100,positiveLimit(limit));
      const page=await this._read('/api/v2/trouble-files?'+new URLSearchParams({cursor,limit:String(limit),sort,direction}),{signal});
      if(page?.sort!==sort||page.direction!==direction)throw new AMCV2Error('File evidence sort did not match the request.',{code:'invalid_response'});
      if(!Array.isArray(page?.files)||page.files.length>limit||page.minSessions!==5||!Number.isSafeInteger(page.totalSessions)||!Number.isSafeInteger(page.indexedSessions)||page.indexedSessions<0||page.indexedSessions>page.totalSessions||typeof page.snapshot!=='string'||!page.snapshot||!Number.isFinite(Date.parse(page.observedAt))||page.nextCursor!=null&&typeof page.nextCursor!=='string'||page.nextCursor&&page.nextCursor===cursor)throw new AMCV2Error('Invalid file evidence page.',{code:'invalid_response'});
      for(const f of page.files)if(typeof f.machineId!=='string'||typeof f.project!=='string'||typeof f.path!=='string'||!f.path||!Number.isSafeInteger(f.sessions)||f.sessions<1||!Number.isSafeInteger(f.badSessions)||f.badSessions<0||!Number.isSafeInteger(f.unknownSessions)||f.unknownSessions<0||f.badSessions+f.unknownSessions>f.sessions||f.rate!==null&&(f.sessions<5||f.unknownSessions!==0||!Number.isFinite(f.rate)||Math.abs(f.rate-f.badSessions*100/f.sessions)>0.000001))throw new AMCV2Error('Invalid per-file rate evidence.',{code:'invalid_response'});
      return page;
    }
    async troubleFileSessions(file,{cursor='',limit=25,signal}={}){
      if(!file||typeof file.machineId!=='string'||!file.machineId||typeof file.project!=='string'||typeof file.path!=='string'||!file.path)throw new AMCV2Error('A recorded file identity is required.',{code:'invalid_filter'});
      limit=Math.min(100,positiveLimit(limit));
      const page=await this._read('/api/v2/trouble-files/sessions?'+new URLSearchParams({machineId:file.machineId,project:file.project,path:file.path,cursor,limit:String(limit)}),{signal});
      if(!Array.isArray(page?.sessions)||page.sessions.length>limit||!Number.isSafeInteger(page.total)||page.total<page.sessions.length||typeof page.snapshot!=='string'||!page.snapshot||!Number.isFinite(Date.parse(page.observedAt))||page.nextCursor!=null&&typeof page.nextCursor!=='string'||page.nextCursor&&page.nextCursor===cursor||page.sessions.some(s=>typeof s.id!=='string'||!s.id||typeof s.bad!=='boolean'||typeof s.unknown!=='boolean'||s.bad&&s.unknown||typeof s.lastTouched!=='string'))throw new AMCV2Error('Invalid file session evidence.',{code:'invalid_response'});
      return page;
    }
    async agentLifecycle(id,agentId,{snapshot='',signal}={}){
      if(typeof agentId!=='string'||!agentId||new TextEncoder().encode(agentId).byteLength>4096)throw new AMCV2Error('A recorded agent identity is required.',{code:'invalid_filter'});
      const query=new URLSearchParams({agentId,...(snapshot?{snapshot}:{})});
      const value=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/agent-lifecycle?'+query,{signal});
      const tri=v=>v===null||typeof v==='boolean';
      if(value?.agentId!==agentId||typeof value.snapshot!=='string'||!value.snapshot||snapshot&&value.snapshot!==snapshot||!Number.isFinite(Date.parse(value.observedAt))||!tri(value.retrying)||!tri(value.stalled)||!Number.isSafeInteger(value.uncertainCalls)||value.uncertainCalls<0||value.pending&&(value.pending.sessionId!==id||value.pending.agentId!==agentId||value.pending.kind!=='tool-call'||!Number.isSafeInteger(value.pending.sequence)||value.pending.sequence<1||value.pending.historySnapshot!==value.snapshot))throw new AMCV2Error('Invalid lifecycle evidence.',{code:'invalid_response'});
      return value;
    }
    async agentTools(id, agentId, options = {}) {
      if(typeof agentId!=='string'||new TextEncoder().encode(agentId).length>4096)throw new AMCV2Error('Invalid agent identity.',{code:'invalid_filter'});
      const limit=positiveLimit(options.limit),query=new URLSearchParams({agentId,limit:String(limit)});
      if(options.cursor)query.set('cursor',options.cursor);
      const page=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/agent-tools?'+query);
      if(!Array.isArray(page?.tools)||page.tools.length>limit||!Number.isSafeInteger(page.throughSequence)||page.throughSequence<0||page.nextCursor!=null&&typeof page.nextCursor!=='string'||page.tools.some(tool=>typeof tool.name!=='string'||!Number.isSafeInteger(tool.calls)||tool.calls<1))throw new AMCV2Error('Invalid tool summary.',{code:'invalid_response'});
      if(page.nextCursor&&page.nextCursor===options.cursor)throw new AMCV2Error('Tool cursor did not advance.',{code:'cursor_not_advancing'});
      return page;
    }
    async stats(id) { return this._read('/api/v2/sessions/' + encodeURIComponent(id) + '/stats'); }
    async fleetGitUndos(options={}) {
	  const project=options.project??null;
	  if(project!==null&&(typeof project!=='string'||project.length>4096))throw new AMCV2Error('Invalid project filter.',{code:'invalid_filter'});
      const limit=positiveLimit(options.limit,25),cursor=options.cursor||'',epoch=this.recoveryEpoch;
      const query=new URLSearchParams({limit:String(limit)});if(cursor)query.set('cursor',cursor);
	  if(project!==null)query.set('project',project);
      const page=await this._request('/api/v2/git-undos?'+query,{signal:options.signal});
	  if((page?.project??null)!==project||project!==null&&Array.isArray(page.events)&&page.events.some(e=>e.project!==project))throw new AMCV2Error('Undo project scope changed.',{code:'invalid_response'});
      const count=n=>Number.isSafeInteger(n)&&n>=0,hash=s=>typeof s==='string'&&/^[a-f0-9]{64}$/.test(s);
      if(epoch!==this.recoveryEpoch||page?.scope!=='all-current-sessions-including-archived'||!hash(page.snapshot)||!Array.isArray(page.events)||page.events.length>limit||typeof (page.nextCursor??'')!=='string'||(page.nextCursor||'').length>2048)throw new AMCV2Error('Invalid fleet undo history.',{code:'invalid_response'});
      if(page.events.some(e=>!count(e?.sequence)||e.sequence<1||!hash(e.sessionSnapshot)||!['sessionId','title','project','machineId','machineName','provider','agentId'].every(k=>typeof e[k]==='string')||!e.sessionId||typeof e.archived!=='boolean'||e.timestamp!==null&&(typeof e.timestamp!=='string'||!Number.isFinite(Date.parse(e.timestamp)))||!['reset','revert','checkout','restore'].includes(e.rule)||typeof e.command!=='string'||e.command.length>443||!count(e.pathCount)||!Array.isArray(e.paths)||e.paths.length!==Math.min(8,e.pathCount)||e.paths.some(p=>typeof p!=='string'||p.length>515))||new Set(page.events.map(e=>e.sequence)).size!==page.events.length)throw new AMCV2Error('Invalid fleet undo event.',{code:'invalid_response'});
      if(page.nextCursor&&(page.nextCursor===cursor||page.events.length!==limit))throw new AMCV2Error('Undo cursor did not advance.',{code:'cursor_not_advancing'});
      return page;
    }
    async priorUndoEdits(id,undoSequence,options={}) {
      const before=options.before??0,limit=positiveLimit(options.limit,25),snapshot=options.snapshot||'',epoch=this.recoveryEpoch;
      if(!Number.isSafeInteger(undoSequence)||undoSequence<1||!Number.isSafeInteger(before)||before<0||before>undoSequence||!snapshot)throw new AMCV2Error('Prior edit context requires a pinned undo record.',{code:'invalid_filter'});
      const query=new URLSearchParams({snapshot,before:String(before),limit:String(limit)});
      const page=await this._request('/api/v2/sessions/'+encodeURIComponent(id)+'/git-undos/'+undoSequence+'/prior-edits?'+query,{signal:options.signal});
      if(epoch!==this.recoveryEpoch||page?.snapshot!==snapshot||page.undoSequence!==undoSequence||page.scope!=='Edit-Write-MultiEdit-NotebookEdit-in-this-session'||!['needs-rebuild','indexed-tool-calls','incomplete-indexing'].includes(page.state)||!Number.isSafeInteger(page.diagnostics)||page.diagnostics<0||!Array.isArray(page.edits)||page.edits.length>limit||!Number.isSafeInteger(page.nextBefore??0)||(page.nextBefore??0)<0)throw new AMCV2Error('Invalid prior edit context.',{code:'invalid_response'});
      let bound=before||undoSequence;
      for(const edit of page.edits){if(!Number.isSafeInteger(edit.sequence)||edit.sequence<1||edit.sequence>=bound||typeof edit.path!=='string'||!edit.path||edit.path.length>4096||!['Edit','Write','MultiEdit','NotebookEdit'].includes(edit.tool))throw new AMCV2Error('Invalid earlier edit evidence.',{code:'invalid_response'});bound=edit.sequence;}
      if(page.state==='needs-rebuild'&&(page.edits.length||page.nextBefore)||page.nextBefore&&(page.edits.length!==limit||page.nextBefore!==bound))throw new AMCV2Error('Prior edit cursor did not advance.',{code:'cursor_not_advancing'});
      return page;
    }
    async gitUndoProjects(options={}) {
      const limit=positiveLimit(options.limit,25),cursor=options.cursor||'',epoch=this.recoveryEpoch;
      const query=new URLSearchParams({limit:String(limit)});if(cursor)query.set('cursor',cursor);
      const page=await this._request('/api/v2/git-undos/projects?'+query,{signal:options.signal});
      const positive=n=>Number.isSafeInteger(n)&&n>0;
      if(epoch!==this.recoveryEpoch||page?.scope!=='all-current-sessions-including-archived'||!/^[a-f0-9]{64}$/.test(page.snapshot||'')||!Array.isArray(page.projects)||page.projects.length>limit||typeof(page.nextCursor??'')!=='string'||(page.nextCursor||'').length>8192||page.projects.some(p=>typeof p.project!=='string'||!positive(p.attempts)||!positive(p.sessions)||p.sessions>p.attempts)||new Set(page.projects.map(p=>p.project)).size!==page.projects.length)throw new AMCV2Error('Invalid undo project ranking.',{code:'invalid_response'});
      if(page.nextCursor&&(page.nextCursor===cursor||page.projects.length!==limit))throw new AMCV2Error('Project cursor did not advance.',{code:'cursor_not_advancing'});
      return page;
    }
    async gitUndoSummary(options={}) {
      const epoch=this.recoveryEpoch,value=await this._request('/api/v2/analytics/git-undos',{signal:options.signal});
      const count=n=>Number.isSafeInteger(n)&&n>=0;
      if(epoch!==this.recoveryEpoch||value?.scope!=='all-current-sessions-including-archived'||!['sessions','needsRebuild','incomplete','ready','attempts','readyAttempts','unsupported','diagnostics'].every(k=>count(value?.[k]))||value.ready+value.needsRebuild+value.incomplete!==value.sessions||value.readyAttempts>value.attempts)throw new AMCV2Error('Invalid undo coverage.',{code:'invalid_response'});
      return value;
    }
    async gitUndos(id,options={}) {
      const after=options.after??0,limit=positiveLimit(options.limit,50),snapshot=options.snapshot||'',epoch=this.recoveryEpoch;
      if(!Number.isSafeInteger(after)||after<0||after>0&&!snapshot)throw new AMCV2Error('Undo continuation requires a snapshot.',{code:'invalid_filter'});
      const query=new URLSearchParams({after:String(after),limit:String(limit)});if(snapshot)query.set('snapshot',snapshot);
      const page=await this._request('/api/v2/sessions/'+encodeURIComponent(id)+'/git-undos?'+query,{signal:options.signal});
      const count=n=>Number.isSafeInteger(n)&&n>=0;
      if(epoch!==this.recoveryEpoch||!['indexed-history','incomplete-indexing','needs-rebuild'].includes(page?.state)||typeof page?.snapshot!=='string'||!/^[a-f0-9]{64}$/.test(page.snapshot)||!['indexedOffset','attempts','unsupported','diagnostics'].every(k=>count(page[k]))||!Array.isArray(page.events)||page.events.length>limit||page.events.length>page.attempts||!count(page.nextSequence??0))throw new AMCV2Error('Invalid undo history.',{code:'invalid_response'});
      if(snapshot&&snapshot!==page.snapshot)throw new AMCV2Error('Undo history changed; reload the first page.',{code:'history_changed'});
      if(page.state==='indexed-history'&&(page.unsupported||page.diagnostics)||page.state==='incomplete-indexing'&&!page.unsupported&&!page.diagnostics||page.state==='needs-rebuild'&&(page.indexedOffset||page.attempts||page.unsupported||page.diagnostics||page.events.length||page.nextSequence))throw new AMCV2Error('Inconsistent undo coverage.',{code:'invalid_response'});
      if(page.events.some((e,i)=>!Number.isSafeInteger(e?.sequence)||e.sequence<=after||i>0&&e.sequence<=page.events[i-1].sequence||!['reset','revert','checkout','restore'].includes(e.rule)||typeof e.command!=='string'||e.command.length>443||!count(e.pathCount)||!Array.isArray(e.paths)||e.paths.length!==Math.min(8,e.pathCount)||e.paths.some(p=>typeof p!=='string'||p.length>515)))throw new AMCV2Error('Invalid undo event.',{code:'invalid_response'});
      if(page.nextSequence&&(page.events.length!==limit||page.nextSequence!==page.events.at(-1).sequence))throw new AMCV2Error('Undo cursor did not advance.',{code:'cursor_not_advancing'});
      return page;
    }
    async modelHookSummary(options={}) {
      const epoch=this.recoveryEpoch,value=await this._request('/api/v2/analytics/hooks/models',{signal:options.signal});
      if(epoch!==this.recoveryEpoch||value?.scope!=='all-current-session-agent-pairs-including-archived'||!['agents','withRecordedModel','withoutRecordedModel','unattributedContexts'].every(k=>Number.isSafeInteger(value?.[k])&&value[k]>=0)||value.withRecordedModel+value.withoutRecordedModel!==value.agents)throw new AMCV2Error('Invalid or replaced model attribution.',{code:'invalid_response'});
      return value;
    }
    async javascriptHookSummary(options={}) {
      const epoch=this.recoveryEpoch,value=await this._request('/api/v2/analytics/hooks/javascript',{signal:options.signal});
      const count=n=>Number.isSafeInteger(n)&&n>=0,e=value?.evidence;
      if(epoch!==this.recoveryEpoch||value?.scope!=='all-current-sessions-including-archived'||!['sessions','needsRebuild','incomplete'].every(k=>count(value?.[k]))||!e||!['sessionsRead','sessionsEdited','sessionsWithErrors','errors'].every(k=>count(e[k]))||e.sessionsRead+value.needsRebuild+value.incomplete!==value.sessions||e.sessionsEdited>e.sessionsRead||e.sessionsWithErrors>e.sessionsEdited||e.errors<e.sessionsWithErrors||typeof value.proposed!=='boolean'||value.proposed!==(e.sessionsEdited>=5&&e.sessionsWithErrors>0)||!Array.isArray(e.examples)||e.examples.length!==Math.min(6,e.sessionsWithErrors)||e.examples.some(x=>typeof x?.sessionId!=='string'||!x.sessionId||!count(x.errors)||x.errors<1)||new Set(e.examples.map(x=>x.sessionId)).size!==e.examples.length||e.examples.reduce((n,x)=>n+x.errors,0)>e.errors)throw new AMCV2Error('Invalid or replaced fleet hook evidence.',{code:'invalid_response'});
      return value;
    }
    async javascriptHookEvidence(id, options={}) {
      const snapshot=options.snapshot||'',epoch=this.recoveryEpoch;
      const query=new URLSearchParams();if(snapshot)query.set('snapshot',snapshot);
      const value=await this._request('/api/v2/sessions/'+encodeURIComponent(id)+'/hook-evidence/javascript?'+query,{signal:options.signal});
      const count=n=>Number.isSafeInteger(n)&&n>=0;
      if(epoch!==this.recoveryEpoch||value?.sessionId!==id||!['indexed-history','incomplete-indexing','needs-rebuild'].includes(value?.state)||typeof value?.snapshot!=='string'||!/^[a-f0-9]{64}$/.test(value.snapshot)||typeof value.edited!=='boolean'||!['indexedOffset','errors','diagnostics'].every(k=>count(value[k]))||value.state==='indexed-history'&&value.diagnostics!==0||value.state==='incomplete-indexing'&&value.diagnostics===0||value.state==='needs-rebuild'&&(value.indexedOffset!==0||value.errors!==0||value.diagnostics!==0||value.edited))throw new AMCV2Error('Invalid or replaced hook evidence.',{code:'invalid_response'});
      if(snapshot&&snapshot!==value.snapshot)throw new AMCV2Error('Hook evidence history changed.',{code:'history_changed'});
      return value;
    }
    async fingerprint(id, options={}) {
      const epoch=this.recoveryEpoch,value=await this._request('/api/v2/sessions/'+encodeURIComponent(id)+'/fingerprint',{signal:options.signal});
      const count=n=>Number.isSafeInteger(n)&&n>=0,bins=a=>Array.isArray(a)&&a.length===24&&a.every(count);
      if(value?.tiers!==undefined&&(!Array.isArray(value.tiers)||value.tiers.length>4||!count(value.recordedTokens)||new Set(value.tiers.map(t=>t.tier)).size!==value.tiers.length||value.tiers.some(t=>!['flagship','premium','mid','cheap'].includes(t.tier)||!count(t.pricedTokens)||t.pricedTokens===0||!Number.isFinite(t.cost)||t.cost<0)||value.tiers.reduce((n,t)=>n+t.pricedTokens,0)>value.recordedTokens))throw new AMCV2Error('Invalid fingerprint tier coverage.',{code:'invalid_response'});
      if(epoch!==this.recoveryEpoch||!value||value.sessionId!==id||!bins(value.buckets)||!bins(value.errors)||!['events','undatedEvents','undatedActivity','undatedErrors','unknownResultStatus','throughSequence'].every(k=>count(value[k]))||value.durationMs!==null&&(!Number.isFinite(value.durationMs)||value.durationMs<0)||value.undatedEvents>value.events||value.buckets.reduce((a,b)=>a+b,0)+value.undatedActivity>value.events)throw new AMCV2Error('Invalid or replaced fingerprint response.',{code:'invalid_response'});
      return value;
    }
    async traceSteps(id,agent,options={}) {
      const after=options.after??0,limit=positiveLimit(options.limit??20),snapshot=options.snapshot||'';
      if(typeof id!=='string'||!id||id.length>4096||typeof agent!=='string'||!agent||agent.length>4096||!Number.isSafeInteger(after)||after<0||limit>100||after>0&&!snapshot)throw new AMCV2Error('Trace pages require a session, agent and pinned continuation.',{code:'invalid_filter'});
      const query=new URLSearchParams({agent,after:String(after),limit:String(limit)});if(snapshot)query.set('snapshot',snapshot);
      const value=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/trace-steps?'+query,{signal:options.signal});
      const states=['known','missing-source','incomplete-evidence','oversize-source','missing-call-identity','unsupported-source','source-record-mismatch','unsupported-arguments','unsupported-provider'];
      const hash=s=>typeof s==='string'&&/^[a-f0-9]{64}$/.test(s);
      if(!value||value.comparison!=='exact-tool-and-json-arguments-v1'||typeof value.completeness!=='string'||!hash(value.snapshot)||!Array.isArray(value.steps)||value.steps.length>limit)throw new AMCV2Error('Invalid trace page.',{code:'invalid_response'});
      this._historySnapshot(value.snapshot,snapshot);
      let previous=after;
      for(const step of value.steps){
        if(!step||!Number.isSafeInteger(step.sequence)||step.sequence<=previous||typeof step.tool!=='string'||typeof step.preview!=='string'||!states.includes(step.state)||(step.state==='known'? !step.tool||!hash(step.signature):!!step.signature))throw new AMCV2Error('Invalid trace step evidence.',{code:'invalid_response'});
        previous=step.sequence;
      }
      const next=value.nextSequence??0;
      if(!Number.isSafeInteger(next)||next<0||next&&(!value.steps.length||next!==previous))throw new AMCV2Error('Trace cursor did not advance.',{code:'cursor_not_advancing'});
      return {...value,nextSequence:next||null};
    }
    async delegationChild(id, anchor, options={}) {
      if(typeof id!=='string'||!id||id.length>4096||!Number.isSafeInteger(anchor)||anchor<1)throw new AMCV2Error('A valid delegation anchor is required.',{code:'invalid_filter'});
      const snapshot=options.snapshot||'',query=new URLSearchParams();if(snapshot)query.set('snapshot',snapshot);
      const value=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/delegations/'+anchor+'/child?'+query,{signal:options.signal});
      const states=['resolved','parent-evidence-incomplete','child-evidence-incomplete','unsupported-provider','not-supported-spawn','call-result-missing-id','call-result-missing-agent','call-result-missing-result','call-result-ambiguous','call-result-invalid-time-or-order','missing-source','oversize-source','source-record-mismatch','unsupported-result','missing-parent-identity','ambiguous-parent','child-not-indexed-or-parent-mismatch','ambiguous-child'];
      const hash=s=>typeof s==='string'&&/^[a-f0-9]{64}$/.test(s);
      if(!value||!states.includes(value.state)||!hash(value.snapshot)||value.resultSequence!=null&&(!Number.isSafeInteger(value.resultSequence)||value.resultSequence<=anchor))throw new AMCV2Error('Invalid child relationship evidence.',{code:'invalid_response'});
      this._historySnapshot(value.snapshot,snapshot);
      if(value.state==='resolved'){
        if(!value.resultSequence||typeof value.childSessionId!=='string'||!value.childSessionId||value.childSessionId===id||!hash(value.childSnapshot)||typeof value.childAgentId!=='string'||!value.childAgentId||value.childAgentId.length>512||value.childAgentId.includes('\0'))throw new AMCV2Error('Incomplete child relationship proof.',{code:'invalid_response'});
      }else if(value.childSessionId||value.childSnapshot||value.childAgentId)throw new AMCV2Error('Unresolved relationship contains an inferred child.',{code:'invalid_response'});
      return value;
    }
    async delegationMatches(id, anchor, options={}) {
      const after=sequence(options.after),limit=Math.min(100,positiveLimit(options.limit??20)),snapshot=options.snapshot||'';
      if(typeof id!=='string'||!id||id.length>4096||!Number.isSafeInteger(anchor)||anchor<1||after>0&&!snapshot)throw new AMCV2Error('A delegation anchor and pinned continuation are required.',{code:'invalid_filter'});
      const query=new URLSearchParams({after:String(after),limit:String(limit)});if(snapshot)query.set('snapshot',snapshot);
      const page=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/delegations/'+anchor+'/matches?'+query,{signal:options.signal});
      const states=['known','needs-reindex','not-delegation','missing-full-text','missing-source','missing-instruction','too-vague','invalid-arguments','oversize','field-limit','ambiguous-unicode'];
      const hash=s=>typeof s==='string'&&/^[a-f0-9]{64}$/.test(s);
      if(!page||!states.includes(page.state)||page.coverage!=='indexed-fingerprints-only'||page.scope!=='other-current-sessions-including-archived'||!hash(page.snapshot)||!Array.isArray(page.matches)||page.matches.length>limit||page.state!=='known'&&page.matches.length)throw new AMCV2Error('Invalid delegation matching evidence.',{code:'invalid_response'});
      this._historySnapshot(page.snapshot,snapshot);
      let previous=after;
      for(const match of page.matches){
        if(!match||!Number.isSafeInteger(match.sequence)||match.sequence<=previous||typeof match.sessionId!=='string'||!match.sessionId||match.sessionId===id||!hash(match.sessionSnapshot)||!['callerAgentId','machineId','provider','title','timestamp'].every(k=>typeof match[k]==='string')||typeof match.archived!=='boolean'||match.timestamp&&!Number.isFinite(Date.parse(match.timestamp)))throw new AMCV2Error('Invalid delegation call reference.',{code:'invalid_response'});
        previous=match.sequence;
      }
      const next=page.nextSequence??0;
      if(!Number.isSafeInteger(next)||next<0||next&&(!page.matches.length||next!==previous||page.state!=='known'))throw new AMCV2Error('Delegation cursor did not advance.',{code:'cursor_not_advancing'});
      return {...page,nextSequence:next||null};
    }
    async toolSpans(id, options={}) {
      const after=options.after??0,limit=positiveLimit(options.limit),snapshot=options.snapshot||'';
      if(!Number.isSafeInteger(after)||after<0||after>0&&!snapshot)throw new AMCV2Error('Continued tool spans require a valid cursor and snapshot.',{code:'invalid_filter'});
      const query=new URLSearchParams({after:String(after),limit:String(limit)});if(snapshot)query.set('snapshot',snapshot);
      const page=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/tool-spans?'+query,{signal:options.signal});
      if(typeof page?.snapshot!=='string'||!/^[a-f0-9]{64}$/.test(page.snapshot))throw new AMCV2Error('Missing tool-span snapshot.',{code:'invalid_response'});
      if(snapshot&&snapshot!==page.snapshot)throw new AMCV2Error('Tool-span history changed.',{code:'history_changed'});
      const states=['matched','missing-id','missing-agent','missing-result','ambiguous','invalid-time-or-order'];
      if(!Array.isArray(page.spans)||page.spans.length>limit||page.spans.some((span,i)=>!span?.call||span.call.sessionId!==id||span.call.kind!=='tool-call'||!Number.isSafeInteger(span.call.sequence)||span.call.sequence<=after||i>0&&span.call.sequence<=page.spans[i-1].call.sequence||!states.includes(span.state)))throw new AMCV2Error('Invalid tool-span page.',{code:'invalid_response'});
      for(const span of page.spans){
        if(span.state==='matched'){
          const start=Date.parse(span.call.timestamp),end=Date.parse(span.endedAt);
          if(!Number.isFinite(start)||!Number.isFinite(end)||end<start||!Number.isSafeInteger(span.durationMillis)||span.durationMillis<0||span.durationMillis!==end-start||!Number.isSafeInteger(span.resultSequence)||span.resultSequence<=span.call.sequence||span.error!=null&&typeof span.error!=='boolean')throw new AMCV2Error('Inconsistent matched tool span.',{code:'invalid_response'});
        }else if(span.durationMillis!=null||span.endedAt!=null||span.resultSequence!=null||span.error!=null)throw new AMCV2Error('Unknown span contains inferred timing or outcome.',{code:'invalid_response'});
      }
      if(page.nextSequence!=null&&page.nextSequence!==0&&(!Number.isSafeInteger(page.nextSequence)||!page.spans.length||page.nextSequence!==page.spans.at(-1).call.sequence))throw new AMCV2Error('Tool-span cursor did not advance.',{code:'cursor_not_advancing'});
      return {...page,nextSequence:page.nextSequence||null};
    }
    async agents(id, options = {}) {
      const page=await this._cursorPage('/api/v2/sessions/' + encodeURIComponent(id) + '/agents', 'agents', options);
      return {...page,agents:page.agents.map(agent=>{
        let name=agent.name,pending=false;
        for(const operation of this.entries.get(id)?.operations||[]) if(operation.patch.agentName?.id===agent.id){name=operation.patch.agentName.name;pending=true;}
        return {...agent,name,namePending:pending};
      })};
    }
    async costFlow(id, options = {}) {
      const limit=positiveLimit(options.limit);
      if(options.cursor&&!options.snapshot)throw new AMCV2Error('Continued cost flow requires its snapshot.',{code:'invalid_cursor'});
      const query=new URLSearchParams({limit:String(limit)});
      for(const key of ['cursor','snapshot'])if(options[key])query.set(key,options[key]);
      const page=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/cost-flow?'+query);
      if(!/^[a-f0-9]{64}$/.test(page?.snapshot||'')||page?.session?.id!==id||!Array.isArray(page.agents)||page.agents.length>limit||page.nextCursor!=null&&typeof page.nextCursor!=='string')throw new AMCV2Error('Invalid cost-flow scope or page.',{code:'invalid_response'});
      this._historySnapshot(page.snapshot,options.snapshot);
      for(const key of ['topCost','topOutput']){
        const ranked=page[key],ids=new Set();
        if(!Array.isArray(ranked)||ranked.length>14||ranked.some((r,i)=>!r||typeof r.id!=='string'||r.name!=null&&typeof r.name!=='string'||!Number.isFinite(r.value)||r.value<=0||i>0&&r.value>ranked[i-1].value||ids.has(r.id)||!ids.add(r.id)))throw new AMCV2Error('Invalid whole-session Flow ranking.',{code:'invalid_response'});
      }
      if(page.nextCursor&&page.nextCursor===options.cursor)throw new AMCV2Error('Cost-flow cursor did not advance.',{code:'cursor_not_advancing'});
      return {...page,nextCursor:page.nextCursor||null};
    }
    async models(id, options = {}) { return this._cursorPage('/api/v2/sessions/' + encodeURIComponent(id) + '/models', 'models', options); }
    agentPages(id, options = {}) { return this._cursorPages(page => this.agents(id, page), options); }
    modelPages(id, options = {}) { return this._cursorPages(page => this.models(id, page), options); }
    async lineage(id, options = {}) {
      const page = await this._cursorPage('/api/v2/sessions/' + encodeURIComponent(id) + '/lineage', 'children', options);
      if (!Array.isArray(page.parents) || page.parents.length > 100) throw new AMCV2Error('The hub did not bound parent references.', { code: 'invalid_response' });
      return page;
    }
    lineagePages(id, options = {}) { return this._cursorPages(page => this.lineage(id, page), options); }
    async eventsAround(id, anchor, options = {}) {
      const side = value => { const n = sequence(value == null ? 50 : value); return Math.min(n, 249); };
      const before = side(options.before), after = side(options.after);
      const page = await this._read('/api/v2/sessions/' + encodeURIComponent(id) + '/events/around?sequence=' + sequence(anchor) + '&before=' + before + '&after=' + after + (options.snapshot ? '&snapshot=' + encodeURIComponent(options.snapshot) : ''), {signal:options.signal});
      if (!Array.isArray(page?.events) || page.events.length > before + after + 1) throw new AMCV2Error('The hub did not bound its replay window.', { code: 'invalid_response' });
      return page;
    }
    _machineLabel(value,id){
      if(!value||value.machineId!==id||typeof value.displayName!=='string'||new TextEncoder().encode(value.displayName).length>1024||!Number.isSafeInteger(value.revision)||value.revision<0||typeof value.updatedAt!=='string'||(value.revision===0?value.updatedAt!=='':!Number.isFinite(Date.parse(value.updatedAt))))throw new AMCV2Error('Invalid machine label response.',{code:'invalid_response'});
      return value;
    }
    _machineLabelOperation(o){
      if(!o||typeof o.machineId!=='string'||!o.machineId||o.machineId.length>1024||o.machineId.includes('\0')||typeof o.displayName!=='string'||new TextEncoder().encode(o.displayName).length>1024||o.displayName.includes('\0')||!Number.isSafeInteger(o.revision)||o.revision<0||typeof o.operationId!=='string'||!/^[A-Za-z0-9_-]{1,128}$/.test(o.operationId)||typeof o.recoveryEpoch!=='string'||!o.recoveryEpoch||o.recoveryEpoch.length>128)throw new AMCV2Error('Invalid machine rename operation.',{code:'invalid_machine_label'});
      return {machineId:o.machineId,displayName:o.displayName,revision:o.revision,operationId:o.operationId,recoveryEpoch:o.recoveryEpoch};
    }
    pendingMachineLabel(id){
      try{
        if(!this.storage)throw Error('No storage');
        const raw=this.storage.getItem(this.key+':machine-label:'+encodeURIComponent(id));
        if(raw==null)return null;
        if(raw.length>8192)throw Error('Oversized intent');
        const o=this._machineLabelOperation(JSON.parse(raw));if(o.machineId!==id)throw Error('Identity mismatch');return o;
      }catch{throw new AMCV2Error('Machine rename recovery storage is unavailable or invalid. Preserve it before recovery.',{code:'blocked_local_storage'});}
    }
    async machineSources(id,{cursor='',limit=100,q='',signal}={}){
      const identity=v=>typeof v==='string'&&v.length>0&&v.length<=1024&&!v.includes('\0');
      if(!identity(id)||!Number.isInteger(limit)||limit<1||limit>100||typeof cursor!=='string'||cursor.length>8192)throw new AMCV2Error('Invalid source catalog request.',{code:'invalid_page'});
      if(typeof q!=='string'||new TextEncoder().encode(q).length>1024||q.includes('\0'))throw new AMCV2Error('Source path filter is too long or invalid.',{code:'invalid_page'});
      const page=await this._read('/api/v2/machines/'+encodeURIComponent(id)+'/sources?limit='+limit+'&cursor='+encodeURIComponent(cursor)+'&q='+encodeURIComponent(q),{signal});
      const invalid=()=>new AMCV2Error('Invalid retained-source catalog.',{code:'invalid_response'});
      if(page?.machineId!==id||(page.query??'')!==q||!Array.isArray(page.items)||page.items.length>limit)throw invalid();
      const seen=new Set();
      for(const item of page.items){
        const s=item?.source,key=JSON.stringify([s?.sourceId,s?.generation]);
        if(item?.title!=null&&typeof item.title!=='string')throw invalid();
        if(!s||s.machineId!==id||!identity(s.sourceId)||!identity(s.generation)||typeof s.path!=='string'||typeof s.provider!=='string'||!Number.isSafeInteger(s.size)||s.size<0||!Number.isSafeInteger(item.durableOffset)||item.durableOffset<0||!Number.isSafeInteger(item.indexedOffset)||item.indexedOffset<0||item.indexedOffset>item.durableOffset||typeof item.activeGeneration!=='boolean'||item.sessionId!=null&&!identity(item.sessionId)||seen.has(key))throw invalid();
        seen.add(key);
      }
      if(page.nextCursor!=null&&(typeof page.nextCursor!=='string'||page.nextCursor.length>8192||page.nextCursor&&(!page.items.length||page.nextCursor===cursor)))throw invalid();
      return page;
    }
    async machineLabel(id){
      if(typeof id!=='string'||!id||id.length>1024||id.includes('\0'))throw new AMCV2Error('A machine identity is required.',{code:'invalid_machine_label'});
      return this._machineLabel(await this._read('/api/v2/machines/'+encodeURIComponent(id)+'/label'),id);
    }
    async machineLabelHistory(id,{after=0,limit=100}={}){
      if(typeof id!=='string'||!id||id.length>1024||id.includes('\0'))throw new AMCV2Error('A machine identity is required.',{code:'invalid_machine_label'});
      after=sequence(after);limit=positiveLimit(limit);
      const page=await this._read('/api/v2/machines/'+encodeURIComponent(id)+'/label/history?after='+after+'&limit='+limit);
      if(!Array.isArray(page?.items)||page.items.length>limit)throw new AMCV2Error('Invalid machine audit page.',{code:'invalid_response'});
      let previous=after;
      for(const item of page.items){
        if(!item||!Number.isSafeInteger(item.revision)||item.revision<=previous||typeof item.operationId!=='string'||!item.operationId||item.operationId.length>1024||typeof item.at!=='string'||!Number.isFinite(Date.parse(item.at)))throw new AMCV2Error('Invalid machine audit entry.',{code:'invalid_response'});
        this._machineLabel(item.before,id);this._machineLabel(item.after,id);
        if(item.after.revision!==item.revision||item.before.revision!==item.revision-1||item.after.updatedAt!==item.at)throw new AMCV2Error('Inconsistent machine audit revisions.',{code:'invalid_response'});
        previous=item.revision;
      }
      if(page.next!=null&&(!Number.isSafeInteger(page.next)||page.next<0||page.next!==0&&(!page.items.length||page.next!==previous)))throw new AMCV2Error('Invalid machine audit cursor.',{code:'invalid_response'});
      return page;
    }
    mutateMachineLabel(operation){
      let o;try{o=this._machineLabelOperation(operation);}catch(error){return Promise.reject(error);}
      const body=JSON.stringify(o),flights=this.machineLabelFlights||(this.machineLabelFlights=new Map()),prior=flights.get(o.machineId);
      if(prior)return prior.body===body?prior.work:Promise.reject(new AMCV2Error('A rename for this machine is already pending.',{code:'machine_label_busy'}));
      const work=this._mutateMachineLabel(o,body);flights.set(o.machineId,{body,work});
      work.finally(()=>{if(flights.get(o.machineId)?.work===work)flights.delete(o.machineId);}).catch(()=>{});return work;
    }
    async _mutateMachineLabel(o,body){
      if(!this.csrf)throw new AMCV2Error('Desktop authentication is required.',{code:'missing_csrf'});
      const key=this.key+':machine-label:'+encodeURIComponent(o.machineId);
      try{
        const pending=this.pendingMachineLabel(o.machineId);
        if(pending&&JSON.stringify(pending)!==body)throw Error('Unresolved intent');
        this.storage.setItem(key,body);if(this.storage.getItem(key)!==body)throw Error('Not persisted');
      }catch{throw new AMCV2Error('Rename intent could not be preserved. No request was sent.',{code:'blocked_local_storage'});}
      const epoch=this.recoveryEpoch;let result;
      try{
        const send=()=>this._request('/api/v2/machines/'+encodeURIComponent(o.machineId)+'/label',{method:'POST',headers:{'Content-Type':'application/json'},body});
        try{result=await send();}
        catch(error){
          if(error.status!==401&&error.status!==403)throw error;
          const fresh=await this.bootstrap();
          if((fresh?.hubId||fresh?.health?.hubId)!==this.hubID||typeof fresh?.csrf!=='string'||!fresh.csrf||(fresh?.recoveryEpoch||fresh?.health?.recoveryEpoch)!==epoch||epoch!==this.recoveryEpoch)throw new AMCV2Error('The original hub and ledger could not be confirmed during authentication recovery. The rename is retained for reconciliation.',{code:'history_changed',status:409});
          result=await send();
        }
      }
      catch(error){
        if(error.code!=='history_changed'&&error.status>=400&&error.status<500&&![401,403,408,429].includes(error.status)){try{if(this.storage.getItem(key)===body)this.storage.removeItem(key);}catch{}}
        throw error;
      }
      this._machineLabel(result,o.machineId);
      if(result.revision!==o.revision+1||result.displayName!==o.displayName.trim()||epoch!==this.recoveryEpoch)throw new AMCV2Error('Rename outcome is uncertain. Retry the saved operation.',{code:'invalid_response',retryable:true});
      try{if(this.storage.getItem(key)===body)this.storage.removeItem(key);}catch{throw new AMCV2Error('Rename acknowledged, but recovery storage could not be cleared.',{code:'blocked_local_storage'});}
      return result;
    }
    async machines() {
      const rows = await this._read('/api/v2/machines');
      if (rows == null) return []; // Older candidate hubs encode an empty slice as null.
      if (!Array.isArray(rows) || rows.length > 1000) throw new AMCV2Error('The machine response exceeds the supported catalog size.', { code: 'invalid_response' });
      return rows;
    }
    async indexingVersions(options = {}) {
      const page = await this._read('/api/v2/indexing/versions?' + this._filters(options));
      if (!Array.isArray(page?.issues) || page.issues.length > positiveLimit(options.limit) || !Number.isInteger(page.checked) || page.checked < 0 || page.checked > positiveLimit(options.limit)
        || page.next != null && typeof page.next !== 'string') throw new AMCV2Error('The hub did not bound its checkpoint audit.', { code: 'invalid_response' });
      if (page.next && page.next === options.cursor) throw new AMCV2Error('The checkpoint cursor did not advance.', { code: 'cursor_not_advancing' });
      return { ...page, nextCursor: page.next || null };
    }
    indexingVersionPages(options = {}) { return this._cursorPages(page => this.indexingVersions(page), options); }
    async organizationHistory(id, options = {}) {
      const after = sequence(options.after), limit = positiveLimit(options.limit);
      const page = await this._read('/api/v2/sessions/' + encodeURIComponent(id) + '/organization/history?after=' + after + '&limit=' + limit);
      if (!Array.isArray(page?.entries) || page.entries.length > limit) throw new AMCV2Error('The organization audit is not bounded.', { code: 'invalid_response' });
      const next = sequence(page.next);
      if (next && next <= after) throw new AMCV2Error('The organization audit cursor did not advance.', { code: 'cursor_not_advancing' });
      return { ...page, nextSequence: next || null };
    }
    async *organizationHistoryPages(id, options = {}) {
      let after = sequence(options.after);
      for (;;) { const page = await this.organizationHistory(id, { ...options, after }); yield page; if (!page.nextSequence) return; after = page.nextSequence; }
    }
    async changeHead() {
      const head = await this._request('/api/v2/changes/head');
      if (!head || typeof head.hubId !== 'string' || !head.hubId || typeof head.recoveryEpoch !== 'string' || !head.recoveryEpoch || !Number.isSafeInteger(head.sequence) || head.sequence < 0)
        throw new AMCV2Error('Invalid change head.', { code: 'invalid_response' });
      if (head.hubId !== this.hubID || head.recoveryEpoch !== this.recoveryEpoch) this.setServerIdentity(head);
      return head;
    }

    async changes(options = {}) {
      const after = sequence(options.after), limit = positiveLimit(options.limit);
      const changes = await this._read('/api/v2/changes?after=' + after + '&limit=' + limit);
      if (!Array.isArray(changes) || changes.length > limit) throw new AMCV2Error('The hub did not bound its change page.', { code: 'invalid_response' });
      let previous = after;
      for (const change of changes) { const next = sequence(change.sequence); if (next <= previous) throw new AMCV2Error('The change cursor did not advance.', { code: 'cursor_not_advancing' }); previous = next; }
      return changes;
    }
    // Full raw history is a browser-managed streaming download, never a JSON
    // fetch or an accumulating array in this client. The URL remains owner-only.
    rawSourceURL(source, generation, offset = 0) { return this.baseURL + '/api/v2/sources/' + encodeURIComponent(source) + '/' + encodeURIComponent(generation) + '/raw?offset=' + sequence(offset); }
    indexedExportURL(id) { return this.baseURL + '/api/v2/sessions/' + encodeURIComponent(id) + '/export.ndjson'; }
    replayBundleURL(id) { return this.baseURL + '/api/v2/sessions/' + encodeURIComponent(id) + '/replay.zip'; }
    async *sessionPages(options = {}) {
      let cursor = options.cursor || null;
      do { const page = await this.sessions({ ...options, cursor }); yield page;
        if (page.nextCursor && page.nextCursor === cursor) throw new AMCV2Error('The hub repeated a session cursor.', { code: 'cursor_not_advancing' });
        cursor = page.nextCursor;
      } while (cursor);
    }
    async events(id, options = {}) {
      const after = sequence(options.after); const limit = positiveLimit(options.limit);
      if (after && !options.snapshot) throw new AMCV2Error('Continued history requires its snapshot.', { code: 'invalid_cursor' });
      let agent='';
      if(Object.hasOwn(options,'agentId')) {
        if(typeof options.agentId!=='string'||new TextEncoder().encode(options.agentId).length>4096)throw new AMCV2Error('Invalid agent identity.',{code:'invalid_filter'});
        agent='&agentId='+encodeURIComponent(options.agentId);
      }
      return this._eventPage('/api/v2/sessions/' + encodeURIComponent(id) + '/events?after=' + after + '&limit=' + limit + agent + (options.snapshot ? '&snapshot=' + encodeURIComponent(options.snapshot) : ''), after, limit, options.snapshot || '', true, {signal:options.signal});
    }
    async inheritance(id) {
      const result=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/inheritance');
      if(!result||!['unverified','matched-baseline'].includes(result.state)||!Number.isSafeInteger(result.pages)||result.pages<0)throw new AMCV2Error('Invalid inheritance evidence.',{code:'invalid_response'});
      if(result.state==='matched-baseline'){
        const m=result.match;
        if(!m||m.childSessionId!==id||typeof m.parentSessionId!=='string'||!m.parentSessionId||m.parentSessionId===id||!['tokensIn','tokensCache','tokensCacheWrite','tokensOut'].every(k=>Number.isSafeInteger(m[k])&&m[k]>=0))throw new AMCV2Error('Invalid inherited baseline.',{code:'invalid_response'});
      }
      return result;
    }
    async reconcileInheritance(id,parentSessionId) {
      if(typeof parentSessionId!=='string'||!parentSessionId||parentSessionId===id)throw new AMCV2Error('A distinct parent session is required.',{code:'invalid_request'});
      const result=await this._request('/api/v2/sessions/'+encodeURIComponent(id)+'/reconcile-inheritance',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({parentSessionId})});
      if(typeof result?.selected!=='boolean'||(result.selected&&(typeof result.proofId!=='string'||!result.proofId||result.inspection?.match?.childSessionId!==id||result.inspection?.match?.parentSessionId!==parentSessionId)))throw new AMCV2Error('Invalid reconciliation receipt; review contribution evidence before retrying.',{code:'invalid_response'});
      return result;
    }
    async contributions(id, options = {}) {
      const limit = Math.min(100, positiveLimit(options.limit));
      if (options.cursor && !options.snapshot) throw new AMCV2Error('Continued contributions require their snapshot.', {code:'invalid_cursor'});
      const query = new URLSearchParams({limit:String(limit)});
      if(options.cursor)query.set('cursor',options.cursor);
      if(options.snapshot)query.set('snapshot',options.snapshot);
      const page=await this._read('/api/v2/sessions/'+encodeURIComponent(id)+'/contributions?'+query,{signal:options.signal});
      const invalid=()=>{throw new AMCV2Error('The hub returned invalid contribution evidence.',{code:'invalid_response'});};
      if(!['verified-duplicate-exclusions-only','verified-usage-exclusions-only'].includes(page?.scope)||!Array.isArray(page.items)||page.items.length>limit)invalid();
      this._historySnapshot(page.snapshot,options.snapshot);
      let previous=options.cursor||'';
      for(const item of page.items){
        const u=item?.recorded;
        if(!u||typeof u.id!=='string'||!u.id||u.id<=previous||u.sessionId!==id||typeof item.counted!=='boolean')invalid();
        if(!['tokensIn','tokensCache','tokensCacheWrite','tokensOut'].every(k=>Number.isSafeInteger(u[k])&&u[k]>=0))invalid();
        if(!item.counted&&(typeof item.excludedByProof!=='string'||!item.excludedByProof||typeof item.ownerSessionId!=='string'||!item.ownerSessionId||item.ownerSessionId===id))invalid();
        if(item.counted&&(item.excludedByProof||item.ownerSessionId||item.exclusionKind))invalid();
        if(!item.counted&&page.scope==='verified-usage-exclusions-only'&&!item.exclusionKind)invalid();
        if(item.exclusionKind&&!['duplicate-codex-usage-v1','fork-baseline-v1'].includes(item.exclusionKind))invalid();
        previous=u.id;
      }
      if(page.nextCursor&&(typeof page.nextCursor!=='string'||!page.items.length||page.nextCursor!==previous||page.nextCursor===options.cursor))invalid();
      return page;
    }
    async usage(id, options = {}) {
      const limit = positiveLimit(options.limit);
      if (options.cursor && !options.snapshot) throw new AMCV2Error('Continued usage requires its snapshot.', { code: 'invalid_cursor' });
      const query = new URLSearchParams({ limit: String(limit) });
      if (options.cursor) query.set('cursor', options.cursor);
      if (options.snapshot) query.set('snapshot', options.snapshot);
      const page = await this._read('/api/v2/sessions/' + encodeURIComponent(id) + '/usage?' + query, {signal:options.signal});
      if (!Array.isArray(page?.observations) || page.observations.length > limit) throw new AMCV2Error('The hub did not return bounded usage.', { code: 'invalid_response' });
      this._historySnapshot(page.snapshot, options.snapshot);
      if (page.nextCursor && page.nextCursor === options.cursor) throw new AMCV2Error('The usage cursor did not advance.', { code: 'cursor_not_advancing' });
      return page;
    }
    async *usagePages(id, options = {}) {
      let cursor = options.cursor, snapshot = options.snapshot;
      for (;;) { const page = await this.usage(id, { ...options, cursor, snapshot }); yield page;
        if (!page.nextCursor) return; cursor = page.nextCursor; snapshot = page.snapshot;
      }
    }
    _historySnapshot(actual, expected) {
      if (typeof actual !== 'string' || !actual || actual.length > 128) throw new AMCV2Error('The hub omitted a valid history snapshot.', { code: 'invalid_response' });
      if (expected && actual !== expected) throw new AMCV2Error('History changed; restart pagination from the first page.', { code: 'history_changed', status: 409 });
    }
    async search(text, options = {}) {
      if (typeof text !== 'string' || !text.trim() || text.length > 4096) throw new AMCV2Error('Search requires between 1 and 4096 characters.', { code: 'invalid_search' });
      if(options.related!==undefined&&typeof options.related!=='boolean')throw new AMCV2Error('Related search mode must be boolean.',{code:'invalid_search'});
      const after = sequence(options.after); const limit = options.related?Math.min(5,positiveLimit(options.limit)):positiveLimit(options.limit);
      if(options.related&&(!options.delegatedOnly||after))throw new AMCV2Error('Related task search requires delegation scope and has no sequence continuation.',{code:'invalid_search'});
      const query = new URLSearchParams({ q: text, after: String(after), limit: String(limit) });
      if(options.related)query.set('mode','related');
      if(options.delegatedOnly!==undefined){
        if(typeof options.delegatedOnly!=='boolean')throw new AMCV2Error('Delegation filter must be boolean.',{code:'invalid_search'});
        query.set('delegatedOnly',String(options.delegatedOnly));
      }
      if(after&&!options.snapshot)throw new AMCV2Error('Continued search requires its snapshot.',{code:'invalid_cursor'});
      if(options.snapshot)query.set('snapshot',options.snapshot);
      if (options.sessionId) query.set('sessionId', options.sessionId);
      return this._eventPage('/api/v2/search?' + query, after, limit,options.snapshot||'',true,{signal:options.signal});
    }
    async _eventPage(path, after, limit, snapshot = '', pinned = false, options = {}) {
      const epoch = this.recoveryEpoch; const page = await this._request(path,options);
      if (epoch !== this.recoveryEpoch) throw stale();
      if (!Array.isArray(page?.events) || page.events.length > limit) throw new AMCV2Error('The hub did not return a bounded event page.', { code: 'invalid_response' });
      if (pinned) this._historySnapshot(page.snapshot, snapshot);
      const next = sequence(page.nextSequence);
      if (next && next <= after) throw new AMCV2Error('The event cursor did not advance.', { code: 'cursor_not_advancing' });
      return { events: page.events, nextSequence: next || null, ...(page.snapshot ? { snapshot: page.snapshot } : {}) };
    }
    async *eventPages(id, options = {}) {
      let after = sequence(options.after), snapshot = options.snapshot;
      for (;;) { const page = await this.events(id, { ...options, after, snapshot }); yield page; if (!page.nextSequence) return; after = page.nextSequence; snapshot = page.snapshot; }
    }
    async *searchPages(text, options = {}) {
      let after = sequence(options.after),snapshot=options.snapshot;
      for (;;) { const page = await this.search(text, { ...options, after,snapshot }); yield page; if (!page.nextSequence) return; after = page.nextSequence;snapshot=page.snapshot; }
    }

    _validatePatch(patch) {
      if (!patch || typeof patch !== 'object' || Array.isArray(patch)) throw new AMCV2Error('Invalid organization change.', { code: 'invalid_patch' });
      const result = {};
      for (const [key, value] of Object.entries(patch)) {
        if (!['archived', 'pinned', 'name', 'note', 'project', 'tags', 'agentName'].includes(key)) throw new AMCV2Error('Unsupported organization field: ' + key, { code: 'invalid_patch' });
        if(key==='agentName'&&(!value||typeof value!=='object'||Array.isArray(value)||Object.keys(value).some(k=>!['id','name'].includes(k))||typeof value.id!=='string'||!value.id||value.id.includes('\0')||typeof value.name!=='string')) throw new AMCV2Error('Agent identity and name must be text.',{code:'invalid_patch'});
        if (['archived', 'pinned'].includes(key) && typeof value !== 'boolean') throw new AMCV2Error(key + ' must be a boolean.', { code: 'invalid_patch' });
        if (['name', 'note', 'project'].includes(key) && typeof value !== 'string') throw new AMCV2Error(key + ' must be text.', { code: 'invalid_patch' });
        if (key === 'tags' && (!Array.isArray(value) || value.some(tag => typeof tag !== 'string'))) throw new AMCV2Error('Tags must be strings.', { code: 'invalid_patch' });
        result[key] = clone(value);
      }
      if (!Object.keys(result).length) throw new AMCV2Error('The organization change is empty.', { code: 'invalid_patch' });
      const bytes = text => new TextEncoder().encode(text).length;
      if(result.agentName&&(bytes(result.agentName.id)>4096||bytes(result.agentName.name)>1024)) throw new AMCV2Error('Agent identity or name exceeds its byte limit.',{code:'invalid_patch'});
      for (const [key, max] of [['name',1024],['project',1024],['note',65536]]) {
        if (own(result,key) && bytes(result[key]) > max) throw new AMCV2Error(key + ' exceeds its ' + max + '-byte limit.', { code: 'invalid_patch' });
      }
      if (result.tags && (result.tags.length > 1000 || result.tags.some(tag => bytes(tag)>1024)))
        throw new AMCV2Error('Use at most 1000 tags, each at most 1024 bytes.', { code: 'invalid_patch' });
      // Reserve room for revision and operation ID in the owner's 64 KiB body.
      if (bytes(JSON.stringify(result)) > 64*1024-256)
        throw new AMCV2Error('This edit exceeds the 64 KiB request limit. Save smaller changes separately; the original values are unchanged.', { code: 'invalid_patch' });
      return result;
    }

    archive(id, archived = true) { return this.organize(id, { archived }); }
    organize(id, patch) {
      if (this.closed) throw new AMCV2Error('The client is closed.', { code: 'closed' });
      patch = this._validatePatch(patch);
      const entry = this._entry(id);
      if (!entry.operations.length && [...this.entries.values()].filter(item => item.operations.length).length >= this.maxPendingSessions) {
        throw new AMCV2Error('The pending-change queue is full. Existing changes are retained; reconnect before adding more.', { code: 'pending_queue_full' });
      }
      const operation = { intentID: this.uuid(), operationID: this.uuid(), patch, request: null, attempts: 0, nextAt: 0, blocked: null };
      const completion = new Promise((resolve, reject) => { operation.resolve = resolve; operation.reject = reject; });
      completion.intentID = operation.intentID;
      entry.operations.push(operation);
      // Deliberately before persistence, fetching, or any awaited work.
      this._emit(id);
      try { this._persist(); }
      catch (error) { operation.blocked = error.code; this._emit(id); this._status(error.code, { sessionID: id, message: error.message }); return completion; }
      queueMicrotask(() => this._pump());
      return completion;
    }
    resume() {
      if (this.closed || this.journalBlocked) return;
      for (const entry of this.entries.values()) {
        for (const operation of entry.operations) { operation.blocked = null; operation.nextAt = 0; operation.authRefreshes = 0; }
        if (entry.operations.length) this._emit(entry.id);
      }
      try { this._persist(); } catch (error) { this._status(error.code, { message: error.message }); return; }
      queueMicrotask(() => this._pump());
    }
    _pump() {
      if (this.closed || this.journalBlocked) return;
      clearTimeout(this.timer); this.timer = null;
      let nextAt = Infinity;
      for (const entry of this.entries.values()) {
        const operation = entry.operations[0];
        if (!operation || entry.running || operation.blocked) continue;
        if (operation.nextAt > this.now()) { nextAt = Math.min(nextAt, operation.nextAt); continue; }
        if (this.activeWrites >= this.maxConcurrentWrites) break;
        entry.running = true; this.activeWrites++;
        void this._write(entry, operation).finally(() => { entry.running = false; this.activeWrites--; this._trim(); this._pump(); });
      }
      if (Number.isFinite(nextAt)) this.timer = setTimeout(() => this._pump(), Math.max(10, nextAt - this.now()));
    }
    async _write(entry, operation) {
      try {
        if (!entry.base) await this.session(entry.id);
        if (!operation.request) {
          operation.request = { ...clone(operation.patch), revision: entry.base.revision, operationId: operation.operationID };
          this._persist(); // exact bytes/identity survive an ambiguous network outcome
        }
        const epoch = this.recoveryEpoch;
        const result = await this._request('/api/v2/sessions/' + encodeURIComponent(entry.id) + '/organization', {
          method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(operation.request),
        });
        if (epoch !== this.recoveryEpoch) throw stale();
        if (!result || !Number.isSafeInteger(result.revision) || result.revision <= operation.request.revision) throw new AMCV2Error('The write response did not confirm a new revision.', { code: 'invalid_response', retryable: true });
        if (!entry.base || result.revision >= entry.base.revision) entry.base = metadata(result);
        entry.operations.shift();
        // A failure removing an already-acknowledged operation only causes a
        // safe idempotent replay after restart; it never invents a new write.
        try { this._persist(); } catch (error) { this._status(error.code, { sessionID: entry.id, message: error.message }); }
        this._emit(entry.id); operation.resolve?.(clone(result));
      } catch (error) {
        if (this.closed) return;
        if ((error.status === 401 || error.status === 403) && !operation.authRefreshes) {
          operation.authRefreshes = 1;
          try {
            const oldCSRF = this.csrf;
            await this.bootstrap();
            if (this.csrf && this.csrf !== oldCSRF) {
              operation.nextAt = 0; this._persist();
              this._status('credentials_refreshed', { sessionID: entry.id }); return;
            }
          } catch (refreshError) { if (refreshError.retryable) error = refreshError; }
        }
        if (error.status === 409) {
          try {
            await this.session(entry.id);
            if (entry.base.revision === operation.request?.revision) throw new AMCV2Error('The server rejected the operation identity without advancing its revision.', { code: 'blocked_conflict' });
            // HTTP409 definitively rejected this exact request. A new revision
            // requires a new operation ID; network retries above never do.
            operation.operationID = this.uuid(); operation.request = null; operation.nextAt = 0;
            this._persist(); this._status('conflict_rebased', { sessionID: entry.id }); return;
          } catch (refreshError) { error = refreshError; }
        }
        if (error.status === 401 || error.status === 403) operation.blocked = 'blocked_auth';
        else if (error.code === 'blocked_local_storage') operation.blocked = error.code;
        else if (!error.retryable) operation.blocked = error.code === 'blocked_conflict' ? error.code : 'blocked_request';
        else {
          operation.attempts++;
          const max = Math.min(this.retryCap, this.retryBase * 2 ** Math.min(operation.attempts - 1, 20));
          operation.nextAt = this.now() + Math.max(Math.round(max * (0.5 + this.random() * 0.5)), error.retryAfter || 0);
        }
        try { this._persist(); } catch { operation.blocked = 'blocked_local_storage'; }
        this._emit(entry.id); this._status(operation.blocked || 'retrying', { sessionID: entry.id, message: error.message, nextAt: operation.nextAt });
      }
    }

    // Bounded pull mode uses the same committed change ledger as SSE. Waiting
    // for the consumer before advancing means a reconnect cannot lose events.
    connectChanges({ onChange = () => {}, pollMs = 1000 } = {}) {
      this.changeController?.abort();
      const controller = new AbortController(); this.changeController = controller;
      const done = (async () => {
        let failures = 0;
        while (!this.closed && !controller.signal.aborted) {
          try {
            const epoch = this.recoveryEpoch;
            const rows = await this._request('/api/v2/changes?after=' + this.changeAfter, { signal: controller.signal });
            if (epoch !== this.recoveryEpoch) continue;
            if (!Array.isArray(rows) || rows.length > 100) throw new AMCV2Error('Invalid bounded change page.', { code: 'invalid_response' });
            for (const change of rows) {
              if (controller.signal.aborted || epoch !== this.recoveryEpoch) break;
              const cursor = sequence(change.sequence);
              if (cursor <= this.changeAfter) continue;
              if (change.sessionId && this.entries.has(change.sessionId)) await this.session(change.sessionId);
              await onChange(clone(change));
              if (epoch !== this.recoveryEpoch) break;
              this.changeAfter = cursor; this._persistCursor();
            }
            failures = 0;
            if (rows.length < 100) await this._pause(Math.max(50, pollMs), controller.signal);
          } catch (error) {
            if (this.closed || controller.signal.aborted) break;
            this._status(error.status === 401 || error.status === 403 ? 'blocked_auth' : 'changes_retrying', { message: error.message });
            failures++;
            await this._pause(Math.max(error.retryAfter || 0, Math.min(this.retryCap, this.retryBase * 2 ** Math.min(failures - 1, 20))), controller.signal);
          }
        }
      })();
      return { close: () => controller.abort(), done };
    }
    _pause(ms, signal) {
      return new Promise(resolve => {
        if (signal.aborted) { resolve(); return; }
        const finish = () => { clearTimeout(timer); signal.removeEventListener('abort', finish); resolve(); };
        const timer = setTimeout(finish, ms); signal.addEventListener('abort', finish, { once: true });
      });
    }
    close() {
      this.closed = true; clearTimeout(this.timer); this.changeController?.abort();
      for (const request of this.requests) request.abort();
    }

    static toLegacySession(row) {
      const m = metadata(row.metadata); const models = Array.isArray(row.models) ? row.models : [];
      return {
        file: 'v2:' + row.id, stableKey: row.id, session: row.nativeId || row.id,
        title: m.name || row.title || row.nativeId || row.id, autoTitle: row.title, renamed: !!m.name,
        machine: row.machineName || row.machineId, kind: row.provider, project: m.projectOverride ? m.project || '' : m.project || row.project,
        mtime: Number.isFinite(Date.parse(row.lastActivity)) ? Date.parse(row.lastActivity) : null,
        events: row.eventCount, tokensIn: row.tokensIn, tokensOut: row.tokensOut,
        cacheTokens: row.tokensCache, cacheWriteTokens: row.tokensCacheWrite,
        cost: row.costEstimate ?? null, costKnown: row.costEstimate != null,
        agents: row.agentCount ?? null, toolCalls: row.toolCalls ?? null, errors: row.errors ?? null,
        durationMs: row.durationMs ?? null, tierMix: row.tierMix ?? null, topTierShare: row.topTierShare ?? null,
        models: models.map(model => typeof model === 'string' ? { id: model, agents: null, cost: null, tier: 'unknown' } : clone(model)),
        // Completeness and current indexing activity are distinct facts.
        completeness: row.completeness, indexing: row.indexing === true,
        parentThreadId: row.parentThreadId || null,
        metadata: { ...m, projectId: m.project || null, pending: !!row.metadata?.pending },
        v2: true, unknownSummaryFields: ['agents', 'toolCalls', 'errors', 'durationMs'].filter(key => row[key === 'agents' ? 'agentCount' : key] == null),
      };
    }
    static toLegacyEvent(event) { return { ...event, seq: event.sequence, agent: event.agentId, ts: event.timestamp,
      rawReference: { offset: event.sourceOffset, length: event.sourceLength }, session: event.sessionId }; }
  }

  return { AMCV2Client, AMCV2Error };
});
