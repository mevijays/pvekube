/* Kafka Training shared engine: navigation, progress, quiz, stepper, copy buttons, partitioner. */
(function () {
  'use strict';
  var MODULES = [
    { n: 1, f: '01-why-kafka.html', t: 'What is Kafka and why it exists', m: 15, g: 'Concepts' },
    { n: 2, f: '02-core-concepts.html', t: 'Core building blocks', m: 25, g: 'Concepts' },
    { n: 3, f: '03-producers.html', t: 'Publishing data (producers)', m: 25, g: 'The data journey' },
    { n: 4, f: '04-replication.html', t: 'Replication and broker failure', m: 30, g: 'The data journey' },
    { n: 5, f: '05-consumers.html', t: 'Consuming data (consumers)', m: 30, g: 'The data journey' },
    { n: 6, f: '06-internals.html', t: 'Inside the cluster', m: 25, g: 'Under the hood' },
    { n: 7, f: '07-kubernetes.html', t: 'Kafka on your Kubernetes', m: 20, g: 'On your Kubernetes' },
    { n: 8, f: '08-cli-lab.html', t: 'Hands-on CLI lab', m: 45, g: 'On your Kubernetes' },
    { n: 9, f: '09-administration.html', t: 'Administration and troubleshooting', m: 40, g: 'On your Kubernetes' },
    { n: 10, f: '10-mastery.html', t: 'Mastery: quiz, cheat sheet, glossary', m: 30, g: 'Mastery' }
  ];

  var store = {
    get: function (k, d) { try { var v = localStorage.getItem('kt-' + k); return v === null ? d : JSON.parse(v); } catch (e) { return d; } },
    set: function (k, v) { try { localStorage.setItem('kt-' + k, JSON.stringify(v)); } catch (e) {} }
  };

  function h(tag, attrs, kids) {
    var e = document.createElement(tag);
    if (attrs) for (var k in attrs) {
      if (k === 'class') e.className = attrs[k]; else if (k === 'html') e.innerHTML = attrs[k];
      else if (k === 'text') e.textContent = attrs[k]; else if (k.slice(0, 2) === 'on') e.addEventListener(k.slice(2), attrs[k]);
      else e.setAttribute(k, attrs[k]);
    }
    (kids || []).forEach(function (c) { if (c) e.appendChild(typeof c === 'string' ? document.createTextNode(c) : c); });
    return e;
  }

  var root = document.body.getAttribute('data-root') || '.';
  var cur = parseInt(document.body.getAttribute('data-module') || '0', 10);
  function href(m) { return root + '/modules/' + m.f; }
  function doneSet() { return store.get('done', {}); }

  function buildSide() {
    var side = document.getElementById('side'); if (!side) return;
    var done = doneSet(), count = Object.keys(done).filter(function (k) { return done[k]; }).length;
    side.innerHTML = '';
    side.appendChild(h('a', { class: 'brand', href: root + '/index.html', html:
      '<svg viewBox="0 0 32 32" aria-hidden="true"><circle cx="16" cy="6" r="4" fill="#534AB7"/><circle cx="6" cy="24" r="4" fill="#0F6E56"/><circle cx="26" cy="24" r="4" fill="#D85A30"/><path d="M16 10 8 21M16 10l8 11M10 24h12" stroke="#888780" stroke-width="1.6" fill="none"/></svg><span>Kafka Training</span>' }));
    side.appendChild(h('p', { class: 'sub', text: 'Zero to cluster admin' }));
    var prog = h('div', { class: 'prog' }, [h('span', { id: 'progtxt', text: count + ' of ' + MODULES.length + ' modules complete' }), h('div', { class: 'bar' }, [h('i', { style: 'width:' + (count / MODULES.length * 100) + '%' })])]);
    side.appendChild(prog);
    var nav = h('nav', { class: 'nav', 'aria-label': 'Modules' }); var lastG = '';
    nav.appendChild(h('a', { href: root + '/index.html', class: cur === 0 ? 'cur' : '', html: '<span class="num">&#8962;</span><span>Start here</span>' }));
    MODULES.forEach(function (m) {
      if (m.g !== lastG) { nav.appendChild(h('div', { class: 'grp', text: m.g })); lastG = m.g; }
      nav.appendChild(h('a', { href: href(m), class: (m.n === cur ? 'cur ' : '') + (done[m.n] ? 'done' : ''), html:
        '<span class="num">' + (done[m.n] ? '&#10003;' : m.n) + '</span><span>' + m.t + '<span class="mins">' + m.m + ' min</span></span>' }));
    });
    side.appendChild(nav);
    var tools = h('div', { class: 'tools' }, [
      h('button', { class: 'btn sm', onclick: function () {
        var t = document.documentElement.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
        document.documentElement.setAttribute('data-theme', t); store.set('theme', t); try { localStorage.setItem('kt-theme', t); } catch (e) {}
      }, text: 'Light / dark' }),
      h('button', { class: 'btn sm', onclick: function () { if (confirm('Reset all progress and quiz scores?')) { try { Object.keys(localStorage).filter(function (k) { return k.indexOf('kt-') === 0 && k !== 'kt-theme'; }).forEach(function (k) { localStorage.removeItem(k); }); } catch (e) {} location.reload(); } }, text: 'Reset progress' })
    ]);
    side.appendChild(tools);
    if (!document.querySelector('.burger')) {
      document.body.appendChild(h('button', { class: 'burger', 'aria-label': 'Menu', text: '☰', onclick: function () { document.body.classList.toggle('nav-open'); } }));
    }
  }

  function buildPager() {
    var main = document.getElementById('main'); if (!main || !cur) return;
    var done = doneSet(), m = MODULES[cur - 1];
    var c = h('div', { class: 'complete' }, [
      h('div', { html: '<b>Finished this module?</b><br><span class="muted small">Mark it complete to track your progress.</span>', style: 'flex:1' }),
      h('button', { class: 'btn primary', id: 'donebtn', text: done[cur] ? 'Completed ✓ (undo)' : 'Mark complete', onclick: function () {
        var d = doneSet(); d[cur] = !d[cur]; store.set('done', d); buildSide();
        this.textContent = d[cur] ? 'Completed ✓ (undo)' : 'Mark complete';
      } })
    ]);
    main.appendChild(c);
    var pg = h('div', { class: 'pager' });
    var p = MODULES[cur - 2], n = MODULES[cur];
    pg.appendChild(p ? h('a', { href: href(p), html: '<small>Previous</small>' + p.t }) : h('a', { href: root + '/index.html', html: '<small>Back</small>Course home' }));
    if (n) pg.appendChild(h('a', { class: 'nx', href: href(n), html: '<small>Next</small>' + n.t }));
    main.appendChild(pg);
  }

  function reveal() {
    var els = document.querySelectorAll('.reveal');
    if (!('IntersectionObserver' in window)) { els.forEach(function (e) { e.classList.add('in'); }); return; }
    var io = new IntersectionObserver(function (en) { en.forEach(function (x) { if (x.isIntersecting) { x.target.classList.add('in'); io.unobserve(x.target); } }); }, { threshold: 0.08 });
    els.forEach(function (e) { io.observe(e); });
  }

  function copyButtons() {
    document.querySelectorAll('pre').forEach(function (pre) {
      if (pre.querySelector('.copy') || pre.hasAttribute('data-nocopy')) return;
      var b = h('button', { class: 'copy', text: 'Copy', onclick: function () {
        var txt = pre.getAttribute('data-copy');
        if (txt === null) { var c = pre.cloneNode(true); c.querySelectorAll('.copy,.c,.o,.p').forEach(function (x) { x.remove(); }); txt = c.textContent; }
        txt = txt.replace(/\n{3,}/g, '\n\n').trim();
        var done = function () { b.textContent = 'Copied'; setTimeout(function () { b.textContent = 'Copy'; }, 1400); };
        if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(txt).then(done, done); else done();
      } });
      pre.appendChild(b);
    });
  }

  function checklists() {
    document.querySelectorAll('.check input[data-id]').forEach(function (i) {
      var k = 'chk-' + i.getAttribute('data-id'); i.checked = !!store.get(k, false);
      i.addEventListener('change', function () { store.set(k, i.checked); });
    });
  }

  /* Quiz: questions = [{q, o:[], a:index, why}] */
  function quiz(el, id, title, qs) {
    var answered = 0, correct = 0;
    el.classList.add('quiz'); el.innerHTML = '';
    el.appendChild(h('h4', { text: title }));
    var best = store.get('quiz-' + id, null);
    var score = h('div', { class: 'score', text: best !== null ? 'Best score: ' + best + ' / ' + qs.length : '' });
    qs.forEach(function (q, qi) {
      var box = h('div', { class: 'q' }); box.appendChild(h('div', { class: 'qt', text: (qi + 1) + '. ' + q.q }));
      /* shuffle answer order on every render so the correct answer's position carries no information */
      var order = q.o.map(function (_, i) { return i; });
      for (var s = order.length - 1; s > 0; s--) { var r = Math.floor(Math.random() * (s + 1)), tmp = order[s]; order[s] = order[r]; order[r] = tmp; }
      order.forEach(function (oi) {
        var o = q.o[oi];
        var inp = h('input', { type: 'radio', name: id + qi });
        var lab = h('label', {}, [inp, h('span', { text: o })]);
        inp.addEventListener('change', function () {
          if (box.classList.contains('done')) return;
          box.classList.add('done'); answered++;
          if (oi === q.a) { lab.classList.add('right'); correct++; }
          else { lab.classList.add('wrong'); box.querySelectorAll('label')[order.indexOf(q.a)].classList.add('right'); }
          box.querySelectorAll('input').forEach(function (x) { x.disabled = true; });
          if (answered === qs.length) {
            var b0 = store.get('quiz-' + id, -1); if (correct > b0) store.set('quiz-' + id, correct);
            score.textContent = 'You scored ' + correct + ' / ' + qs.length + (correct === qs.length ? ' - perfect!' : ' - review the explanations above and retry later.');
          }
        });
        box.appendChild(lab);
      });
      box.appendChild(h('div', { class: 'why', html: '<b>Why:</b> ' + q.why }));
      el.appendChild(box);
    });
    el.appendChild(score);
    el.appendChild(h('button', { class: 'btn sm', text: 'Retry quiz', onclick: function () { quiz(el, id, title, qs); } }));
  }

  /* Stepper: steps = [{t, d}], apply(i) updates the visual */
  function stepper(el, steps, apply, opt) {
    opt = opt || {}; var i = 0, timer = null;
    var info = h('div', { class: 'info' }); var dots = h('div', { class: 'dots' });
    steps.forEach(function () { dots.appendChild(h('i')); });
    var prev = h('button', { class: 'btn sm', text: 'Back', onclick: function () { stop(); go(i - 1); } });
    var next = h('button', { class: 'btn sm primary', text: 'Next step', onclick: function () { stop(); go(i + 1); } });
    var play = h('button', { class: 'btn sm', text: 'Auto-play', onclick: function () { if (timer) stop(); else { start(); } } });
    var rst = h('button', { class: 'btn sm', text: 'Restart', onclick: function () { stop(); go(0); } });
    el.appendChild(h('div', { class: 'stepper-ctl' }, [prev, next, play, rst, dots, info]));
    function stop() { if (timer) { clearInterval(timer); timer = null; play.textContent = 'Auto-play'; } }
    function start() { play.textContent = 'Pause'; if (i >= steps.length - 1) go(0); timer = setInterval(function () { if (i >= steps.length - 1) { stop(); return; } go(i + 1); }, opt.ms || 2800); }
    function go(n) {
      i = Math.max(0, Math.min(steps.length - 1, n));
      info.innerHTML = '<b>Step ' + (i + 1) + ' of ' + steps.length + ': ' + steps[i].t + '</b>' + steps[i].d;
      dots.querySelectorAll('i').forEach(function (d, k) { d.classList.toggle('on', k === i); });
      prev.disabled = i === 0; next.disabled = i === steps.length - 1;
      apply(i, steps[i]);
    }
    go(0); return { go: go, stop: stop };
  }

  /* Kafka's default partitioner: murmur2(key) & 0x7fffffff % partitions. Verified against the lab cluster. */
  function murmur2(data) {
    var length = data.length, m = 0x5bd1e995, r = 24, hh = (0x9747b28c ^ length) | 0, n4 = length >> 2, i, j, k;
    for (i = 0; i < n4; i++) {
      j = i * 4; k = (data[j] & 0xff) | ((data[j + 1] & 0xff) << 8) | ((data[j + 2] & 0xff) << 16) | ((data[j + 3] & 0xff) << 24);
      k = Math.imul(k, m); k ^= k >>> r; k = Math.imul(k, m); hh = Math.imul(hh, m); hh ^= k;
    }
    var t = length & ~3;
    switch (length % 4) {
      case 3: hh ^= (data[t + 2] & 0xff) << 16; /* falls through */
      case 2: hh ^= (data[t + 1] & 0xff) << 8; /* falls through */
      case 1: hh ^= (data[t] & 0xff); hh = Math.imul(hh, m);
    }
    hh ^= hh >>> 13; hh = Math.imul(hh, m); hh ^= hh >>> 15; return hh | 0;
  }
  function partitionFor(key, n) { return (murmur2(new TextEncoder().encode(String(key))) & 0x7fffffff) % n; }

  function logTo(box, msg, cls) {
    var d = h('div', { class: cls || '', text: msg }); box.appendChild(d); box.scrollTop = box.scrollHeight;
    while (box.children.length > 60) box.removeChild(box.firstChild);
  }

  window.KT = { h: h, store: store, quiz: quiz, stepper: stepper, partitionFor: partitionFor, murmur2: murmur2, log: logTo, MODULES: MODULES };
  document.addEventListener('DOMContentLoaded', function () { buildSide(); buildPager(); reveal(); copyButtons(); checklists(); });
  if (document.readyState !== 'loading') { buildSide(); buildPager(); reveal(); copyButtons(); checklists(); }
})();
