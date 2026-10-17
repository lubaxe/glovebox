'use strict';

// The setup page: where clips are backed up (with a connection test) and
// the extra drives. It only needs /api/setup and style.css, so other pages
// can show it too, like a sign-in page on the Pi's own Wi-Fi network:
//
//   GloveboxSetup.mount(container, { onSaved })

(function () {
  function h(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (v === undefined || v === null || v === false) continue;
      if (k === 'class') node.className = v;
      else if (k === 'style') node.style.cssText = v;
      else if (k.startsWith('on')) node.addEventListener(k.slice(2), v);
      else if (k === 'value') node.value = v;
      else if (k === 'checked') node.checked = !!v;
      else node.setAttribute(k, v === true ? '' : v);
    }
    for (const c of children.flat()) {
      if (c === null || c === undefined || c === false) continue;
      node.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return node;
  }

  async function request(method, path, body) {
    const opts = { method, headers: {} };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(path, opts);
    const text = await res.text();
    let data = {};
    try { data = JSON.parse(text); } catch { /* plain-text error */ }
    if (!res.ok) throw new Error(data.error || text.trim() || `HTTP ${res.status}`);
    return data;
  }

  const TYPES = [
    { id: 'smb', name: 'SMB share', sub: 'Most NAS boxes: Unraid, Synology, TrueNAS, QNAP, Windows' },
    { id: 'nfs', name: 'NFS export', sub: 'NAS boxes and Linux servers' },
    { id: 'rsync', name: 'rsync over SSH', sub: 'Any server you can log in to with SSH' },
    { id: 'rclone', name: 'rclone', sub: 'Google Drive, OneDrive, Dropbox, S3, B2, SFTP, WebDAV and more' },
  ];

  const SOURCE_HINT = {
    smb: 'A share, or a folder in one: e.g. Music or TeslaCam/Music.',
    nfs: 'An export path on the same server, e.g. /mnt/user/Music.',
    rsync: 'A folder on the same server, e.g. /volume1/Music.',
    rclone: 'A folder on the same remote, e.g. Music, or another remote as name:folder.',
  };

  const DRIVES = [
    { id: 'music', name: 'Music', sizes: ['4G', '8G', '16G', '32G', '64G'], suggest: '8G', source: 'Music',
      text: 'Songs the car plays from USB. Glovebox copies the folder over whenever the car gets home.' },
    { id: 'lightshow', name: 'Light Show', sizes: ['512M', '1G', '2G'], suggest: '1G', source: 'LightShow',
      text: 'Custom light shows: an .fseq file and an .mp3 or .wav with the same name. They go in the drive\'s LightShow folder.' },
    { id: 'boombox', name: 'Boombox', sizes: ['128M', '256M', '512M'], suggest: '256M', source: 'Boombox',
      text: 'Sounds for Boombox, as .mp3 or .wav. They go in the drive\'s Boombox folder.' },
  ];

  function field(label, input, hint) {
    return h('label', { class: 'field' }, h('span', { class: 'field-label' }, label), input, hint ? h('span', { class: 'field-hint' }, hint) : null);
  }

  // text binds a text input to obj[key].
  function text(obj, key, attrs = {}) {
    return h('input', { type: 'text', value: obj[key] ?? '', autocomplete: 'off', spellcheck: 'false', ...attrs, oninput: (e) => { obj[key] = e.target.value; } });
  }

  function check(obj, key, label) {
    return h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: obj[key], onchange: (e) => { obj[key] = e.target.checked; } }), h('span', {}, label));
  }

  function result(state, msg) {
    const mark = state === 'busy' ? h('span', { class: 'spinner' }) : h('span', { class: 'mark' }, state === 'ok' ? '✓' : '✕');
    return h('div', { class: `result ${state}` }, mark, h('span', {}, msg));
  }

  async function busy(button, out, msg, fn) {
    button.disabled = true;
    out.replaceChildren(result('busy', msg));
    try {
      const done = await fn();
      out.replaceChildren(...(Array.isArray(done) ? done : [result('ok', done)]));
    } catch (e) {
      out.replaceChildren(result('bad', e.message));
    } finally {
      button.disabled = false;
    }
  }

  function step(num, title, intro, ...body) {
    return h('div', { class: 'step' },
      h('span', { class: 'step-num' }, String(num)),
      h('div', {}, h('h3', {}, title), intro ? h('p', {}, intro) : null, ...body));
  }

  async function mount(root, opts = {}) {
    root.replaceChildren(h('p', { class: 'muted' }, 'Loading…'));
    let s;
    try {
      s = await request('GET', '/api/setup');
    } catch (e) {
      root.replaceChildren(h('p', { class: 'result bad' }, `Couldn't load the settings: ${e.message}`));
      return;
    }
    const a = s.archive;
    if (!a.rsync.port) a.rsync.port = 22;
    if (!a.enabled && !a.smb.server && !a.nfs.server && !a.rsync.server && !a.rclone.remote) a.enabled = true;

    // ---- 1. type ----
    const typeBox = h('div', { class: 'type-cards', role: 'radiogroup' });
    const connBox = h('div');
    function renderTypes() {
      typeBox.replaceChildren(...TYPES.map((t) => h('button', {
        type: 'button', role: 'radio', 'aria-checked': String(a.type === t.id),
        class: `type-card ${a.type === t.id ? 'selected' : ''}`,
        onclick: () => { a.type = t.id; renderTypes(); renderConn(); renderDrives(); },
      }, h('b', {}, t.name), h('span', {}, t.sub))));
    }

    // ---- 2. connection ----
    const testOut = h('div', { class: 'check-steps' });
    const testBtn = h('button', { type: 'button' }, 'Test the connection');
    testBtn.addEventListener('click', () => busy(testBtn, testOut, 'Connecting…', async () => {
      const r = await request('POST', '/api/setup/test', s);
      return r.steps.map((st) => result(st.ok ? 'ok' : 'bad', st.detail ? `${st.name}: ${st.detail}` : st.name));
    }));

    function checkField(required) {
      const hint = required
        ? `An address that only answers at home: while it does, the car is home and Glovebox backs up. Your router is a good choice${s.gateway ? `, e.g. ${s.gateway}:80` : ' (e.g. 192.168.1.1:80)'}.`
        : 'Optional. By default the server itself tells Glovebox the car is home. host:port';
      return field(required ? 'Home check address' : 'Home check address (advanced)',
        text(a, 'check_address', { placeholder: s.gateway ? `${s.gateway}:80` : '192.168.1.1:80' }), hint);
    }

    function renderConn() {
      const t = a.type;
      const parts = [];
      if (t === 'smb') {
        const m = a.smb;
        parts.push(h('div', { class: 'fields' },
          field('Server', text(m, 'server', { placeholder: '192.168.1.10' }), 'The NAS\'s address or name.'),
          field('Share', text(m, 'share', { placeholder: 'TeslaCam' })),
          field('Folder in the share', text(m, 'folder', { placeholder: 'optional' })),
          field('Username', text(m, 'username', { placeholder: 'carcam' })),
          field('Password', text(m, 'password', { type: 'password', placeholder: m.password_set ? 'unchanged' : '' }),
            m.password_set ? 'Leave empty to keep the saved one.' : null)),
        h('details', { class: 'advanced' }, h('summary', {}, 'Advanced'),
          h('div', { class: 'fields' },
            field('SMB version', h('select', { onchange: (e) => { m.version = e.target.value; } },
              [['2.1', '2.1 (fastest on a Pi Zero 2 W)'], ['3.0', '3.0'], ['3.0.2', '3.0.2'], ['3.1.1', '3.1.1'], ['', 'Negotiate']]
                .map(([v, l]) => h('option', { value: v, selected: m.version === v }, l)))),
            checkField(false))));
      } else if (t === 'nfs') {
        const m = a.nfs;
        parts.push(h('div', { class: 'fields' },
          field('Server', text(m, 'server', { placeholder: '192.168.1.10' })),
          field('Export', text(m, 'export', { placeholder: '/mnt/user/TeslaCam' }), 'The exported folder\'s path on the server. It must allow writes from the Pi.'),
          field('Folder in the export', text(m, 'folder', { placeholder: 'optional' }))),
        h('details', { class: 'advanced' }, h('summary', {}, 'Advanced'), h('div', { class: 'fields' }, checkField(false))));
      } else if (t === 'rsync') {
        const m = a.rsync;
        const port = h('input', { type: 'number', min: 1, max: 65535, value: m.port, oninput: (e) => { m.port = Number(e.target.value) || 22; } });
        const keyOut = h('div');
        const keyBox = h('div', { class: 'keybox' });
        const showKey = () => {
          if (!m.public_key) {
            const btn = h('button', { type: 'button' }, 'Create Glovebox\'s SSH key');
            btn.addEventListener('click', () => busy(btn, keyOut, 'Creating a key…', async () => {
              m.public_key = (await request('POST', '/api/setup/ssh-key', {})).public_key;
              showKey();
              return 'Created.';
            }));
            keyBox.replaceChildren(h('p', {}, 'Glovebox logs in with its own SSH key, without a password.'), btn, keyOut);
            return;
          }
          const copy = h('button', { type: 'button', class: 'btn-quiet' }, 'Copy');
          copy.addEventListener('click', async () => {
            try { await navigator.clipboard.writeText(m.public_key); copy.textContent = 'Copied'; } catch { copy.textContent = 'Select and copy it'; }
          });
          keyBox.replaceChildren(
            h('p', {}, 'Add this line to ', h('code', {}, '~/.ssh/authorized_keys'), ` of ${m.user || 'the user'} on the server:`),
            h('pre', { class: 'pubkey' }, m.public_key), copy);
        };
        showKey();
        parts.push(h('div', { class: 'fields' },
          field('Server', text(m, 'server', { placeholder: '192.168.1.10' })),
          field('SSH port', port),
          field('User', text(m, 'user', { placeholder: 'carcam' })),
          field('Folder on the server', text(m, 'path', { placeholder: '/volume1/TeslaCam' }))),
        keyBox,
        h('details', { class: 'advanced' }, h('summary', {}, 'Advanced'), h('div', { class: 'fields' }, checkField(false))));
      } else {
        const m = a.rclone;
        let [remote, ...rest] = (m.remote || '').split(':');
        let folder = rest.join(':') || (m.remote ? '' : 'TeslaCam');
        const sync = () => { m.remote = remote ? `${remote}:${folder}` : ''; };
        const remotes = h('select', { onchange: (e) => { remote = e.target.value; sync(); } });
        const fillRemotes = () => {
          if (m.remotes.length && !m.remotes.includes(remote)) remote = m.remotes[0];
          remotes.replaceChildren(...(m.remotes.length ? m.remotes : ['']).map((r) => h('option', { value: r, selected: r === remote }, r || 'Paste an rclone.conf first')));
          remotes.disabled = !m.remotes.length;
          sync();
        };
        fillRemotes();
        const conf = h('textarea', { rows: 6, spellcheck: 'false', placeholder: '[gdrive]\ntype = drive\ntoken = {...}' });
        const confOut = h('div');
        const confBtn = h('button', { type: 'button' }, 'Save rclone.conf');
        confBtn.addEventListener('click', () => busy(confBtn, confOut, 'Saving…', async () => {
          m.remotes = (await request('PUT', '/api/setup/rclone-config', { config: conf.value })).remotes;
          fillRemotes();
          conf.value = '';
          return `Saved. Remotes: ${m.remotes.join(', ')}.`;
        }));
        const confBlock = h('div', { class: 'conf' },
          h('p', {}, 'Set up the remote on a computer with ', h('code', {}, 'rclone config'),
            ' (it signs in to cloud services in a browser), then paste the contents of its rclone.conf (',
            h('code', {}, 'rclone config file'), ' shows where it is).'),
          conf, h('div', { class: 'row' }, confBtn), confOut);
        parts.push(
          m.remotes.length ? h('details', { class: 'advanced' }, h('summary', {}, 'Replace rclone.conf'), confBlock) : confBlock,
          h('div', { class: 'fields' },
            field('Remote', remotes),
            field('Folder on the remote', h('input', { type: 'text', value: folder, placeholder: 'TeslaCam', oninput: (e) => { folder = e.target.value; sync(); } })),
            checkField(true)));
      }
      parts.push(check(a, 'recent', 'Also back up all driving footage (Recent clips, about 15-20 GB per hour of driving)'));
      parts.push(h('div', { class: 'row', style: 'margin-top:14px' }, testBtn), testOut);
      connBox.replaceChildren(...parts);
      testOut.replaceChildren();
    }

    // ---- 3. extra drives ----
    const drivesBox = h('div', { class: 'drive-list' });
    function renderDrives() {
      drivesBox.replaceChildren(...DRIVES.map((d) => {
        const v = s.drives[d.id];
        if (!v.size) v.size = '0';
        const on = v.size !== '0';
        const sizes = d.sizes.includes(v.size) || !on ? d.sizes : [...d.sizes, v.size];
        const source = text(v, 'source', { placeholder: d.source, disabled: !on });
        const size = h('select', {
          onchange: (e) => {
            v.size = e.target.value;
            if (v.size !== '0' && !v.source) v.source = d.source;
            renderDrives();
          },
        }, h('option', { value: '0', selected: !on }, 'Off'), sizes.map((z) => h('option', { value: z, selected: v.size === z }, z.replace('G', ' GB').replace('M', ' MB'))));
        return h('div', { class: `drive-row ${on ? '' : 'off'}` },
          h('div', {}, h('h4', {}, d.name), h('p', {}, d.text)),
          h('div', { class: 'fields' }, field('Size', size), field('Copied from', source, SOURCE_HINT[a.type])));
      }));
    }

    // ---- 4. save ----
    const saveOut = h('div');
    const saveBtn = h('button', { type: 'button', class: 'btn-primary' }, 'Save and restart');
    saveBtn.addEventListener('click', () => busy(saveBtn, saveOut, 'Saving…', async () => {
      const r = await request('PUT', '/api/setup', s);
      if (!r.restarting) return 'Saved. Restart Glovebox to use the new settings.';
      saveOut.replaceChildren(result('busy', 'Saved. Glovebox is restarting…'));
      await new Promise((ok) => setTimeout(ok, 3000));
      for (let i = 0; i < 40; i++) {
        try {
          await request('GET', '/api/status');
          break;
        } catch { await new Promise((ok) => setTimeout(ok, 1500)); }
      }
      if (opts.onSaved) opts.onSaved();
      return 'Saved. Glovebox restarted with the new settings.';
    }));

    renderTypes();
    renderConn();
    renderDrives();
    // other pages add their own steps before and after: functions of the
    // step's number
    let n = 0;
    const next = () => ++n;
    const before = (opts.before || []).map((f) => f(next()));
    const saveStep = step(0, 'Save', 'Glovebox restarts to use the new settings. The dashcam drive stays connected, unless the extra drives changed: then the car sees all of them reconnect.',
      h('div', { class: 'row' }, saveBtn), saveOut);
    let middle;
    if (opts.fold && a.enabled) {
      // set up before: show what's there, and open only what's to change
      saveStep.hidden = true;
      const opened = () => { saveStep.hidden = false; renumber(root); };
      const on = DRIVES.filter((d) => s.drives[d.id].size && s.drives[d.id].size !== '0');
      middle = [
        folded(next(), 'Backups', `Backing up to ${describe(a)}${a.recent ? ', with all driving footage' : ''}.`, 'Change',
          () => [typeBox, check(a, 'enabled', 'Back up when the car is home'), connBox], opened),
        folded(next(), 'Extra drives', on.length
          ? `${on.map((d) => `${d.name} (${s.drives[d.id].size.replace('G', ' GB').replace('M', ' MB')}, from ${s.drives[d.id].source || 'nowhere'})`).join(', ')}.`
          : 'No extra drives.', on.length ? 'Change' : 'Add a drive', () => [drivesBox], opened),
      ];
    } else {
      middle = [
        step(next(), 'Where should clips go?', 'Glovebox copies Sentry and saved clips there whenever the car is home, and nothing leaves your network unless you pick a cloud remote.',
          typeBox, check(a, 'enabled', 'Back up when the car is home')),
        step(next(), 'Connect', null, connBox),
        step(next(), 'Extra drives', 'Besides the dashcam drive, the car can get drives with music, a light show and Boombox sounds. Their files are copied from your server, and updated whenever the car gets home. The dashcam drive keeps the rest of the space.',
          drivesBox),
      ];
    }
    saveStep.querySelector('.step-num').textContent = String(next());
    root.replaceChildren(h('div', { class: 'steps setup' },
      before, middle, saveStep,
      (opts.after || []).map((f) => f(next()))));
    renumber(root);
  }

  // renumber numbers the steps that show, in order; done ones keep their
  // check mark.
  function renumber(root) {
    let i = 0;
    for (const st of root.querySelectorAll('.step')) {
      if (st.hidden) continue;
      i++;
      if (!st.classList.contains('done')) st.querySelector('.step-num').textContent = String(i);
    }
  }

  // describe names the archive target in a few words.
  function describe(a) {
    switch (a.type) {
      case 'smb': return `//${a.smb.server}/${a.smb.share}${a.smb.folder ? `/${a.smb.folder}` : ''}`;
      case 'nfs': return `${a.nfs.server}:${a.nfs.export}${a.nfs.folder ? `/${a.nfs.folder}` : ''}`;
      case 'rsync': return `${a.rsync.user}@${a.rsync.server}:${a.rsync.path}`;
      default: return a.rclone.remote;
    }
  }

  // folded is a step that's done: a summary, and a button that opens what
  // body returns, to change it.
  function folded(num, title, summary, label, body, onOpen) {
    const sum = summary instanceof Node ? summary : h('p', {}, summary);
    const content = h('div', {}, h('h3', {}, title), sum);
    const mark = h('span', { class: 'step-num' }, '✓');
    const wrap = h('div', { class: 'step done' }, mark, content);
    const btn = h('button', { type: 'button' }, label);
    btn.addEventListener('click', () => {
      // opened, it's a step to do again: numbered, with the form
      btn.parentElement.remove();
      wrap.classList.remove('done');
      mark.textContent = String(num);
      content.append(...body());
      if (onOpen) onOpen();
      const steps = wrap.closest('.steps');
      if (steps) renumber(steps);
    });
    content.append(h('div', { class: 'row' }, btn));
    return wrap;
  }


  // ---- first-run wizard ----

  const COUNTRIES = [['US', 'United States'], ['CA', 'Canada'], ['GB', 'United Kingdom'], ['AU', 'Australia'], ['NZ', 'New Zealand'],
    ['DE', 'Germany'], ['FR', 'France'], ['NL', 'Netherlands'], ['NO', 'Norway'], ['SE', 'Sweden'], ['CH', 'Switzerland'],
    ['ES', 'Spain'], ['IT', 'Italy'], ['JP', 'Japan'], ['CN', 'China']];

  function doneStep(num, done, title, intro, ...body) {
    return h('div', { class: `step ${done ? 'done' : ''}` },
      h('span', { class: 'step-num' }, done ? '✓' : String(num)),
      h('div', {}, h('h3', {}, title), intro ? h('p', {}, intro) : null, ...body));
  }

  const sleep = (ms) => new Promise((ok) => setTimeout(ok, ms));

  function wifiStep(wifi, refresh) {
    return (num) => {
      const out = h('div');
      const where = (w) => h('p', {},
        `Glovebox is on ${w.ssid}. Open `, h('a', { href: `http://${w.hostname}.local:8080` }, `http://${w.hostname}.local:8080`),
        w.address ? [' or ', h('a', { href: `http://${w.address}:8080` }, `http://${w.address}:8080`)] : null,
        ' from any device on that network.');
      const form = { ssid: '', password: '', country: '' };
      const other = h('input', { type: 'text', placeholder: 'Network name', autocomplete: 'off', spellcheck: 'false', oninput: (e) => { form.ssid = e.target.value; } });
      const otherField = field('Network name', other);
      const list = h('select', { onchange: (e) => { form.ssid = e.target.value; otherField.hidden = form.ssid !== ''; } });
      const scan = async () => {
        list.replaceChildren(h('option', { value: '' }, 'Scanning…'));
        try {
          const nets = await request('GET', '/api/wifi/networks');
          list.replaceChildren(...nets.map((n) => h('option', { value: n.ssid }, `${n.ssid}${n.secure ? '' : ' (open)'}`)), h('option', { value: '' }, 'Other network…'));
          form.ssid = nets.length ? nets[0].ssid : '';
          otherField.hidden = form.ssid !== '';
        } catch (e) {
          list.replaceChildren(h('option', { value: '' }, 'Other network…'));
          out.replaceChildren(result('bad', `Scan failed: ${e.message}`));
        }
      };
      const country = h('select', { onchange: (e) => { form.country = e.target.value; } },
        h('option', { value: '' }, 'Unchanged'), COUNTRIES.map(([c, name]) => h('option', { value: c }, name)));
      const join = h('button', { type: 'button', class: 'btn-primary' }, 'Connect');
      join.addEventListener('click', () => busy(join, out, 'Connecting… On the setup network, your phone may drop off for a moment.', async () => {
        const sent = Date.now();
        let res;
        try {
          res = await request('POST', '/api/wifi/join', form);
        } catch {
          // the phone lost the setup network while the radio changed channel
          for (let i = 0; i < 45 && !res; i++) {
            await sleep(2000);
            try {
              const st = await request('GET', '/api/wifi');
              if (st.last_join && new Date(st.last_join.at).getTime() >= sent - 5000) res = st.last_join;
            } catch { /* still reconnecting */ }
          }
          if (!res) throw new Error('Lost touch with Glovebox. Reconnect to its network and reload this page.');
        }
        if (!res.ok) throw new Error(res.error);
        setTimeout(refresh, 1500);
        return `Connected to ${res.ssid}${res.address ? ` as ${res.address}` : ''}.`;
      }));
      const formNodes = () => [
        h('div', { class: 'fields' },
          field('Network', h('div', { class: 'row' }, list, h('button', { type: 'button', class: 'btn-quiet', onclick: scan }, 'Scan'))),
          otherField,
          field('Password', h('input', { type: 'password', autocomplete: 'off', oninput: (e) => { form.password = e.target.value; } }), 'Leave empty for an open network. The Pi Zero 2 W only does 2.4 GHz.'),
          field('Country', country, 'Sets the Wi-Fi channels allowed where you are.')),
        h('div', { class: 'row' }, join), out];
      if (wifi.connected) {
        // on Wi-Fi already: nothing to do unless it's to change
        return folded(num, 'Wi-Fi', where(wifi), 'Change Wi-Fi', () => {
          scan();
          return [h('p', {}, 'Joining another network moves Glovebox there. If this device isn\'t on that network too, this page loses touch with Glovebox: open it again from that network.'), ...formNodes()];
        });
      }
      scan();
      return doneStep(num, false, 'Wi-Fi', wifi.access_point
        ? `Your home network, so Glovebox can back up when the car is home. Once it's connected, you can carry on here, or switch back to your home Wi-Fi and open http://${wifi.hostname}.local:8080.`
        : 'Your home network, so Glovebox can back up when the car is home.',
        wifi.connected ? where(wifi) : null, ...formNodes());
    };
  }

  function carStep(car) {
    return (num) => {
      if (!car) {
        return doneStep(num, false, 'Your car', 'Keep-awake is off in the settings, so there\'s nothing to pair.');
      }
      const out = h('div');
      const vin = h('input', { type: 'text', class: 'vin-input', value: car.vin || '', placeholder: '17 characters', maxlength: 17, autocomplete: 'off', spellcheck: 'false' });
      const save = h('button', { type: 'button' }, 'Save');
      save.addEventListener('click', () => busy(save, out, 'Saving…', async () => {
        await request('PUT', '/api/car/vin', { vin: vin.value });
        car.vin = vin.value;
        return 'Saved.';
      }));
      return doneStep(num, car.paired, 'Your car',
        car.paired ? 'Paired: Glovebox keeps the car awake while it backs up what it recorded away from home.'
          : 'A sleeping car cuts the Pi\'s power, so Glovebox pairs with the car over Bluetooth to keep it awake while it backs up. Its key can wake the car but not unlock or drive it.',
        car.paired ? h('p', {}, h('a', { href: '#/car' }, 'Pair again or forget the car on the Car page')) : [
          field('VIN', h('div', { class: 'row' }, vin, save), 'In the car under Controls → Software, or in the Tesla app.'), out,
          h('p', {}, 'Pairing takes a minute in the car with a key card: ', h('a', { href: '#/car' }, 'pair on the Car page'), ', now or later.')]);
    };
  }

  function finishStep(onFinish) {
    return (num) => {
      const out = h('div');
      const btn = h('button', { type: 'button', class: 'btn-primary' }, 'Finish setup');
      btn.addEventListener('click', () => busy(btn, out, 'Finishing…', async () => {
        await request('POST', '/api/setup/finish', {});
        if (onFinish) setTimeout(onFinish, 1200);
        return 'Done. If you set up over Glovebox\'s own network, it turns off now: switch back to your home Wi-Fi.';
      }));
      return doneStep(num, false, 'Done', 'You can change all of this later on the Setup and Car pages.', h('div', { class: 'row' }, btn), out);
    };
  }

  // wizard shows the first-run setup: Wi-Fi, then the setup page's steps,
  // then the car.
  async function wizard(root, opts = {}) {
    root.replaceChildren(h('p', { class: 'muted' }, 'Loading…'));
    const [wifi, car] = await Promise.all([
      request('GET', '/api/wifi').catch(() => null),
      request('GET', '/api/car').catch(() => null)]);
    const again = () => wizard(root, opts);
    const rerun = wifi?.setup_done;
    const skip = h('button', { type: 'button', class: 'btn-quiet' }, rerun ? 'Close' : 'Skip setup');
    skip.addEventListener('click', async () => {
      await request('POST', '/api/setup/finish', {}).catch(() => {});
      if (opts.onFinish) opts.onFinish();
    });
    const box = h('div');
    root.replaceChildren(h('div', { class: 'welcome' },
      h('div', {},
        h('h2', {}, rerun ? 'Change your setup' : 'Welcome to Glovebox'),
        h('p', {}, rerun ? 'Everything set up already is folded: open just the parts you want to change, and save.'
          : 'A few steps, and your Sentry and saved clips back themselves up whenever the car gets home.')),
      h('div', { class: 'row' }, rerun ? h('a', { class: 'btn btn-quiet', href: '#/settings' }, 'All settings') : null, skip)), box);
    await mount(box, {
      before: wifi ? [wifiStep(wifi, again)] : [],
      after: rerun ? [carStep(car)] : [carStep(car), finishStep(opts.onFinish)],
      onSaved: again,
      fold: true,
    });
  }

  window.GloveboxSetup = { mount, wizard };
}());
