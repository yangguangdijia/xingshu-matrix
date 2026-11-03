/* 星枢 Matrix 登录页 WebGL 动效层：粒子漂浮 / 光轨流动 / 晶体旋转 / 鼠标视差。
   不支持 WebGL 或用户偏好减少动效时直接退出，由 CSS 动画兜底。 */
(function () {
  "use strict";
  var auth = document.getElementById("auth");
  if (!auth) return;
  if (window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;

  var cv = document.createElement("canvas");
  cv.className = "xs-gl";
  cv.setAttribute("aria-hidden", "true");
  var gl = cv.getContext("webgl", { alpha: true, antialias: true, premultipliedAlpha: true, powerPreference: "low-power" });
  if (!gl) return;

  function sh(type, src) {
    var s = gl.createShader(type);
    gl.shaderSource(s, src);
    gl.compileShader(s);
    if (!gl.getShaderParameter(s, gl.COMPILE_STATUS)) { gl.deleteShader(s); return null; }
    return s;
  }
  function prog(vs, fs) {
    var v = sh(gl.VERTEX_SHADER, vs), f = sh(gl.FRAGMENT_SHADER, fs);
    if (!v || !f) return null;
    var p = gl.createProgram();
    gl.attachShader(p, v); gl.attachShader(p, f); gl.linkProgram(p);
    return gl.getProgramParameter(p, gl.LINK_STATUS) ? p : null;
  }

  /* ---------- 粒子 + 晶体（点精灵）---------- */
  var P = prog(
    "attribute vec4 a;attribute vec3 b;uniform float t;uniform vec2 m;uniform float k;uniform float dpr;varying float vA;varying float vK;varying float vR;" +
    "void main(){float ph=a.w;float sp=b.x;float x=a.x+sin(t*.25*sp+ph*6.28)*.03;" +
    "float y=fract(a.y+t*.012*sp+ph);float depth=a.z;" +
    "vec2 pos=vec2(x*2.-1.,y*2.-1.)+m*depth*.06;" +
    "gl_Position=vec4(pos,0.,1.);gl_PointSize=b.y*dpr*(.6+depth);" +
    "vA=smoothstep(0.,.1,y)*(1.-smoothstep(.85,1.,y))*(.45+.55*sin(t*1.3+ph*40.)*.5+.275);vK=b.z;vR=t*.35*sp+ph*6.28;}",
    "precision mediump float;varying float vA;varying float vK;varying float vR;" +
    "void main(){vec2 c=gl_PointCoord-.5;" +
    "if(vK>.5){float s=sin(vR),co=cos(vR);c=vec2(c.x*co-c.y*s,c.x*s+c.y*co);" +
    "float d=max(abs(c.x)*.866+abs(c.y)*.5,abs(c.y));d*=1.12;" +
    "float edge=smoothstep(.5,.46,d);float rim=smoothstep(.38,.47,d)*edge;" +
    "float a1=dot(normalize(c+vec2(.001)),vec2(.7,.7));float f=floor((atan(c.y,c.x)+3.1416)/1.0472);" +
    "float facet=.55+.45*sin(f*2.1+vR*1.5);float core=smoothstep(.0,.35,.5-d);" +
    "vec3 base=mix(vec3(.25,.5,1.),vec3(.62,.55,1.),fract(f*.37));" +
    "vec3 col=mix(base*(.6+.7*facet),vec3(.9,.97,1.),rim*.9+core*.18*a1);" +
    "gl_FragColor=vec4(col*edge*vA*.55,edge*vA*.4);return;}" +
    "float d=length(c);float g=smoothstep(.5,0.,d);g*=g;gl_FragColor=vec4(vec3(.65,.88,1.)*g*vA*1.4,g*vA);}"
  );

  /* ---------- 光轨（全屏片元着色）---------- */
  var L = prog(
    "attribute vec2 p;void main(){gl_Position=vec4(p,0.,1.);}",
    "precision mediump float;uniform vec2 r;uniform float t;uniform vec2 m;" +
    "float trail(vec2 uv,float ang,float off,float spd,float w,float seed){" +
    "float s=sin(ang),c=cos(ang);vec2 q=vec2(uv.x*c+uv.y*s,-uv.x*s+uv.y*c);" +
    "float d=abs(q.y-off);float line=exp(-d*d/(w*w*2.2));" +
    "float head=fract(t*spd+seed);float along=q.x*.5+.5;float dist=fract(head-along);" +
    "float tail=exp(-dist*5.)*step(dist,.5)*smoothstep(0.,.04,dist);return line*tail;}" +
    "void main(){vec2 uv=gl_FragCoord.xy/r;uv.x*=r.x/r.y;uv=uv*2.-vec2(r.x/r.y,1.);uv+=m*.04;" +
    "float a=trail(uv,-.38,-.55,.05,.006,.0)*1.0+trail(uv,-.30,-.18,.04,.004,.37)*.7+" +
    "trail(uv,-.46,.28,.065,.005,.61)*.8+trail(uv,-.22,.62,.035,.003,.83)*.55+trail(uv,-.52,-.9,.045,.004,.2)*.6;" +
    "a=min(a*1.9,1.);vec3 col=mix(vec3(.35,.7,1.),vec3(.9,.97,1.),a);gl_FragColor=vec4(col*a,a*.9);}"
  );
  if (!P || !L) return;

  /* 粒子数据：a=(x,y,depth,phase) b=(speed,size,kind) */
  var N = 90, K = 14, total = N + K, data = new Float32Array(total * 7), seed = 20261102;
  function rnd() { seed = (seed * 1664525 + 1013904223) >>> 0; return seed / 4294967296; }
  for (var i = 0; i < total; i++) {
    var kind = i >= N ? 1 : 0, o = i * 7;
    data[o] = rnd(); data[o + 1] = rnd(); data[o + 2] = .3 + rnd() * .9; data[o + 3] = rnd();
    data[o + 4] = .5 + rnd() * 1.1;
    data[o + 5] = kind ? 12 + rnd() * 16 : 4 + rnd() * 9;
    data[o + 6] = kind;
  }
  var pbuf = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, pbuf);
  gl.bufferData(gl.ARRAY_BUFFER, data, gl.STATIC_DRAW);
  var qbuf = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, qbuf);
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]), gl.STATIC_DRAW);

  var uP = { t: gl.getUniformLocation(P, "t"), m: gl.getUniformLocation(P, "m"), dpr: gl.getUniformLocation(P, "dpr") };
  var uL = { r: gl.getUniformLocation(L, "r"), t: gl.getUniformLocation(L, "t"), m: gl.getUniformLocation(L, "m") };
  var aA = gl.getAttribLocation(P, "a"), aB = gl.getAttribLocation(P, "b"), aQ = gl.getAttribLocation(L, "p");

  var dpr = 1, mx = 0, my = 0, tx = 0, ty = 0, raf = 0, t0 = performance.now(), running = false;
  function resize() {
    dpr = Math.min(window.devicePixelRatio || 1, 2);
    var w = Math.round(window.innerWidth * dpr), h = Math.round(window.innerHeight * dpr);
    if (cv.width !== w || cv.height !== h) { cv.width = w; cv.height = h; }
    gl.viewport(0, 0, w, h);
  }
  function frame(now) {
    raf = 0;
    if (!running) return;
    var t = (now - t0) / 1000;
    mx += (tx - mx) * .06; my += (ty - my) * .06;
    gl.clearColor(0, 0, 0, 0);
    gl.clear(gl.COLOR_BUFFER_BIT);
    gl.enable(gl.BLEND);
    gl.blendFunc(gl.ONE, gl.ONE_MINUS_SRC_ALPHA);

    gl.useProgram(L);
    gl.bindBuffer(gl.ARRAY_BUFFER, qbuf);
    gl.enableVertexAttribArray(aQ);
    gl.vertexAttribPointer(aQ, 2, gl.FLOAT, false, 0, 0);
    gl.uniform2f(uL.r, cv.width, cv.height);
    gl.uniform1f(uL.t, t);
    gl.uniform2f(uL.m, mx, my);
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4);
    gl.disableVertexAttribArray(aQ);

    gl.useProgram(P);
    gl.bindBuffer(gl.ARRAY_BUFFER, pbuf);
    gl.enableVertexAttribArray(aA);
    gl.vertexAttribPointer(aA, 4, gl.FLOAT, false, 28, 0);
    gl.enableVertexAttribArray(aB);
    gl.vertexAttribPointer(aB, 3, gl.FLOAT, false, 28, 16);
    gl.uniform1f(uP.t, t);
    gl.uniform2f(uP.m, mx, my);
    gl.uniform1f(uP.dpr, dpr);
    gl.drawArrays(gl.POINTS, 0, total);
    gl.disableVertexAttribArray(aA);
    gl.disableVertexAttribArray(aB);
    raf = requestAnimationFrame(frame);
  }
  function start() { if (running) return; running = true; t0 = performance.now() - (t0 ? 0 : 0); raf = requestAnimationFrame(frame); }
  function stop() { running = false; if (raf) cancelAnimationFrame(raf); raf = 0; }
  function sync() {
    var visible = !auth.hidden && !document.hidden;
    if (visible) { resize(); start(); } else stop();
  }

  function onMove(e) {
    var w = window.innerWidth || 1, h = window.innerHeight || 1;
    tx = (e.clientX / w - .5) * 2; ty = -(e.clientY / h - .5) * 2;
    var layer = auth.querySelector(".xs-anim-layer");
    if (layer) { layer.style.setProperty("--px", (-tx * 10).toFixed(1) + "px"); layer.style.setProperty("--py", (ty * 8).toFixed(1) + "px"); }
  }
  window.addEventListener("pointermove", onMove, { passive: true });
  window.addEventListener("resize", resize);
  document.addEventListener("visibilitychange", sync);
  cv.addEventListener("webglcontextlost", function (e) { e.preventDefault(); stop(); auth.classList.remove("xs-gl-on"); });
  cv.addEventListener("webglcontextrestored", function () { location.reload(); });
  new MutationObserver(sync).observe(auth, { attributes: true, attributeFilter: ["hidden"] });

  auth.insertBefore(cv, auth.firstChild);
  auth.classList.add("xs-gl-on");
  sync();
})();
