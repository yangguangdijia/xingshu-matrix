# 登录页排版修复：右侧竖排标语偏小 + 页脚版权位置漂移

适用范围：`public/index.html`（内联 `<style>` 中的 `.x-auth` 登录页样式块 + 登录页 DOM）
提交：`4dc4fcd fix: enlarge right-side vertical note on login and tie footer to the form card`

---

## 一、报错问题

用户在 `http://localhost:4173/#/login` 截图指出两处问题（红框标注）：

1. **右侧竖排标语「让运维更简单」太小**。用户反馈原话大致是「右侧这栏字我不是说有点小吗，你怎么没改啊」——说明上一轮已经提过，但我只把 `font-size` 从 `11px` 调到 `13px`，用户仍然认为没改。
2. **页面右下角那行「© 星枢 Matrix · 私有化部署版 v1.0」位置不对**，用户直接问「你觉得下面这行字位置对吗」。

改前实测（视口 `1600×900`，`devicePixelRatio=1`）：

| 元素 | 选择器 | 字号 | 颜色 | 盒子 | 与参照物的关系 |
| --- | --- | --- | --- | --- | --- |
| 竖排标语 | `.x-aside` | `13px` | `#7c93b0`（`rgb(124,147,176)`） | `20 × 98`，`right: 6px` | 距右边缘 6px，6 个字总长仅 98px |
| 页脚版权 | `.x-bcopy` | `12px` | `#7c8da3` | `188 × 19` | 在卡片正下方 **58px**（视口更高时可达 130px+），靠 `justify-self: end` 对齐列尾 |
| 登录卡片 | `.x-formp` | — | — | `520 × 642`，`t=152, b=794` | 在 `grid-template-rows: minmax(0,1fr) auto` 的第一行内垂直居中 |

视觉表现：一条 13px、浅灰蓝（`#7c93b0`，对浅蓝底对比度极低）的竖排小字贴在屏幕最右缘，远看几乎是一条虚线；版权行孤零零落在页面右下角，与卡片之间有几十到上百 px 的空白，只靠「右边缘刚好和卡片对齐」这一条弱关系维系，看起来像掉在角落里的备注。

---

## 二、原因

### 2.1 竖排标语「改了还是小」的三层原因

1. **改动幅度不够**：历史提交 `c7bd64b` 把竖排字从 `12px` 改到 `11px`（更小），提交 `db7eb52` 又调到 `13px`。相对正文 `14px`，`13px` 依然是整页最小的字；竖排 + `letter-spacing: .26em` 后单个字形本身很小，视觉上比同 px 的横排字更弱。
2. **对比度才是「看不清」的主因**：`#7c93b0` 在 `#eef4fc ~ #e4eefb` 的浅蓝渐变底上，明度差过小（相当于约 2:1 的对比），即使放大到 14~15px 也依旧是「灰雾」。只调字号不调颜色，主观感受几乎不变——这是用户觉得「你怎么没改啊」的直接原因。
3. **装饰件没跟着放大**：图标 `16px`、竖线 `40px`、间距按 11px 的字设计，字号一提就会显出头重脚轻，所以我上一轮不敢往大写，形成了「改不动」的循环。

### 2.2 版权行位置漂移的原因

`.x-ashell` 是二维网格：

```css
.x-ashell {
  display: grid;
  grid-template-columns: minmax(0, 1fr) 520px;
  grid-template-rows: minmax(0, 1fr) auto;  /* ← 问题根源 */
  align-items: center;
  min-height: 100vh;
}
```

- 卡片 `.x-formp` 在第 1 行，行高 `minmax(0,1fr)`，`align-items: center` 让它**在剩余空间里垂直居中**；
- 版权 `.x-bcopy` 是 `grid-column: 1 / -1` 的第 2 行（`auto` 行），被推到**网格底部**。

两行之间没有任何绑定关系，于是「卡片居中、版权贴底」，屏幕越高，两者之间的空白越大（`1280×720` 时 18px，`1600×900` 时 58px，更高视口 130px 以上）。版权行既不属于卡片，也不属于页面页脚，处于「半吊子」位置——这就是它看起来不对的原因。

---

## 三、解决方案

### 3.1 竖排标语：字号 + 字重 + 字色 + 装饰件整体放大

```css
/* public/index.html 内联样式，约第 1625 行 */
.x-aside {
  display: block; position: fixed; right: 10px; top: 50%;
  transform: translateY(-50%); z-index: 2;
  width: 32px; margin: 0;
  color: #40597f;            /* 原 #7c93b0 */
  font-size: 18px;           /* 原 13px */
  font-weight: 500;          /* 新增，中文细体竖排更飘 */
  letter-spacing: .3em;      /* 原 .26em */
  line-height: 30px;         /* 原 20px */
  text-align: center;
  writing-mode: vertical-rl;
}
.x-aside svg { position: absolute; top: -36px; left: 50%; width: 20px; height: 20px; margin-left: -10px; fill: #2f5fe0; }
.x-aside::before, .x-aside::after { content: ""; position: absolute; left: 50%; width: 1px; height: 48px; background: linear-gradient(180deg, transparent, #c3d3e6); }
.x-aside::before { top: -92px; }
.x-aside::after  { bottom: -92px; background: linear-gradient(0deg, transparent, #c3d3e6); }
```

逐参数说明：

| 参数 | 改前 | 改后 | 作用 |
| --- | --- | --- | --- |
| `right` | `6px` | `10px` | 宽度从 20 加到 32 后，整体仍离屏幕右缘 10px，字形居中在 `32px` 盒内 |
| `width` | `20px` | `32px` | 容纳 18px 字形 + 竖排对齐，避免 `text-align: center` 时贴边 |
| `font-size` | `13px` | `18px` | 6 个字总长由 98px → 140px，占 900px 视口高度约 15.6%，远看是一个明确的竖排标签 |
| `font-weight` | 400（继承） | `500` | 中文细体在浅底上发虚，加半档字重 |
| `color` | `#7c93b0` | `#40597f` | 明度差从约 2:1 提到约 5:1，这是「看得清」的关键，比单纯加字号有效 |
| `letter-spacing` | `.26em` | `.3em` | 竖排字距随字号同比微调，避免 18px 下显得挤 |
| `line-height` | `20px` | `30px` | 单行竖排时为行盒下限，同时给图标留出呼吸位 |
| 图标 `width/height` | `16px` | `20px` | 与字号同比（约 1.1 倍字高） |
| 图标 `top` | `-38px` | `-36px` | 图标底边距文字顶端 16px |
| 竖线 `height` | `40px` | `48px` | 装饰线随整体放大，长度约为图标与文字的合计间距 |
| `::before top` | `-86px` | `-92px` | 线与图标之间留 8px 断口（`-92+48=-44`，图标在 `-36`） |
| `::after bottom` | `-86px` | `-92px` | 下方对称，保持视觉平衡 |

断点策略不变：`@media (max-width: 1359px) { .x-aside { display: none; } }`，`1180px` 以下同样隐藏，避免窄屏与卡片打架。实测 `1360×768` 时标语左边缘距卡片右边缘仍有 14px。

### 3.2 版权行：改为「卡片列内、卡片正下方」的从属元素

DOM 由两兄弟改成一层包裹（约第 1687 行）：

```html
<!-- 改前 -->
<div class="x-formp"><div class="x-aform" id="acard"></div></div>
<p class="x-bcopy">© 星枢 Matrix · 私有化部署版 v1.0</p>

<!-- 改后 -->
<div class="x-formcol">
  <div class="x-formp"><div class="x-aform" id="acard"></div></div>
  <p class="x-bcopy">© 星枢 Matrix · 私有化部署版 v1.0</p>
</div>
```

配套 CSS：

```css
.x-formcol { justify-self: end; width: 100%; max-width: 520px; display: flex; flex-direction: column; align-items: stretch; }
.x-bcopy { align-self: flex-end; margin: 16px 2px 0; color: #7c8da3; font-size: 12px; line-height: 1.6; }
```

- `.x-formcol` 承接原来 `.x-formp` 在网格里的定位职责（`justify-self: end` + `max-width: 520px`）；
- `.x-formp` 的 `max-width: 520px` 保留作兜底，在 flex 列里等价；
- `.x-bcopy` 去掉 `grid-column` / `justify-self`，改为 `align-self: flex-end`，**永远贴着卡片下边缘、16px 间距、右对齐卡片右边缘（内缩 2px 与描边/圆角对齐）**；
- `.x-ashell` 的 `grid-template-rows` 从 `minmax(0, 1fr) auto` 改为 `minmax(0, 1fr)`，删掉那条只为放版权而存在的空行。

响应式补丁（按断点逐个核对）：

| 断点 | 追加规则 | 原因 |
| --- | --- | --- |
| `max-height: 820px` | `.x-bcopy { margin-top: 10px; }`；`.x-ashell { padding-bottom: 16px → 6px }` | 卡片 + 版权合计高度在 720px 高的视口里会溢出 13px，出现滚动条；收紧后 `scrollHeight` 由 733 回到 720 |
| `max-width: 900px` | `.x-formcol { justify-self: stretch; max-width: none; }`、`.x-bcopy { align-self: center; text-align: center; }` | 单列布局下卡片占满宽度，版权右对齐会显得吊在右边；移动端改为居中 |
| `max-width: 600px` | 沿用 `.x-bcopy { margin-top: 22px; }` | 小屏留白稍大 |

### 3.3 改后实测（同一 page，逐视口量 `getBoundingClientRect()`）

| 视口 | 标语 | 标语与卡片间距 | 版权距卡片底 | `scrollHeight` | 结论 |
| --- | --- | --- | --- | --- | --- |
| `1760×1000` | `32×140`，18px，`#40597f` | 94px | 16px | 1000（无滚动） | 正常 |
| `1600×900` | `32×140` | — | 16px | 900 | 正常 |
| `1440×900` | `32×140` | 14px | 16px | 900 | 紧凑但不重叠 |
| `1360×768` | `32×140` | 14px | 10px | 768 | 正常 |
| `1280×720` | 隐藏（`<1359px`） | — | 10px | 720（无滚动条） | 正常 |
| `390×844` | 隐藏 | — | 19px，居中 | 1221（正常滚动） | 正常 |

---

## 四、如何避免

1. **「太小」不要只调 `font-size`**。竖排浅色小字的主因通常是**对比度**：先算一下字色与底色的明度差（`#7c93b0` 对 `#eef4fc` 约 2:1，正文级至少 4.5:1），颜色不到位时，字号 +2px 用户基本无感。改动同时给出「字号/字重/字色」三项的最终值，而不是只调一项。
2. **一次到位，不要 11px → 13px 这样试探**。用户明确说了「这栏字小」，就该按「明显变大」的量级改（本次 13 → 18px，约 +38%），并在回复里给出改前/改后的 px 数值，让用户可验证。
3. **装饰件（图标、竖线、字距、行盒）必须跟着字号同比例换算**，否则放大文字会破坏对齐。本次按约 1.1 倍字高重算图标 `20px`、竖线 `48px`、断口 8px。
4. **grid 里「跨列贴底」的元素最容易变成孤儿**。判断标准：这个元素和谁有语义关系？有，就把它和那个元素**放进同一个 flex 列/同一个 box**，而不是靠 `grid-column: 1 / -1` + `justify-self: end` 制造「看起来对齐」的巧合。空出来的 `auto` 行要一并删掉。
5. **每个断点都要实测，不要只改主样式**：`getBoundingClientRect()` 量 `card.bottom`、`copy.top` 的差，再量 `document.querySelector('.x-auth').scrollHeight` 与 `innerHeight`；相减为正就是在溢出（本次就靠这个抓到 720px 高度下多出的 13px 滚动条）。
6. **本地预览必须绕过缓存**。`python -m http.server` 会因浏览器缓存返回旧页面，改完样式却在页面上看不到变化，很容易误判「没生效」。验证时用带版本参数的 URL（如 `?v=5#/login` 配合 `navigate --force`），或直接 `curl` 服务端文件确认新规则真的在响应里。
7. **登录页样式集中在 `public/index.html` 的内联 `<style>` 里，没有独立 CSS 文件**，两次出现过「改了别的文件、页面没变」的情况；以后改登录页，先 `grep -n "x-aside\|x-bcopy" public/index.html` 确认改的就是被页面引用的那一段。
