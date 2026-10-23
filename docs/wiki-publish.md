# 发布使用手册（VitePress）

`wiki/` 是使用手册的**内容源**，同时也是 VitePress 站点的根目录；构建后得到纯静态文件，可发布到任意静态站点。

## 1. 本地预览与构建

```bash
cd wiki
npm install          # 首次
npm run dev          # 本地预览 http://localhost:5173（热更新）
npm run build        # 构建到 wiki/.vitepress/dist
npm run preview      # 预览构建产物
```

产物目录：`wiki/.vitepress/dist`（已在 `.gitignore` 中忽略，不要提交）。

## 2. 发布到 Nginx

把 `dist/` 传到服务器（示例路径 `/var/www/mocenter-wiki`），然后：

```nginx
server {
    listen 80;
    server_name docs.example.com;
    root /var/www/mocenter-wiki;
    index index.html;

    # VitePress 默认 cleanUrls=false，页面地址带 .html
    location / {
        try_files $uri $uri/ $uri.html =404;
    }

    # 带哈希的静态资源可长缓存
    location /assets/ {
        expires 30d;
        add_header Cache-Control "public, immutable";
    }

    gzip on;
    gzip_types text/css application/javascript application/json image/svg+xml;
    gzip_min_length 1024;
}
```

需要 HTTPS 时按常规方式配置证书（或前置 CDN/反代）。

## 3. 部署到子路径（如 `/mocenter/`）

编辑 `wiki/.vitepress/config.mjs`：

```js
export default defineConfig({
  base: '/mocenter/',   // 必须前后都带斜杠
  // ...
})
```

重新 `npm run build`，把 `dist/` 放到站点根下的 `mocenter/` 目录即可。

## 4. 发布到对象存储 / CDN

`dist/` 里全是静态文件，直接整目录上传即可：

- 默认索引：`index.html`
- 404 处理：部分平台需要配置“404 → /index.html”或返回 404，不要回退到首页（会掩盖错误链接）
- MIME：确保 `.svg`、`.css`、`.js` 正确；多数平台已内置

## 5. 更新流程

```bash
# 1) 改 wiki/*.md（保留 📷 截图占位，交付前统一补图）
# 2) 本地预览确认
cd wiki && npm run dev
# 3) 构建并同步
npm run build
rsync -av --delete .vitepress/dist/ user@host:/var/www/mocenter-wiki/
```

## 6. 注意事项

- **不要提交** `wiki/node_modules/`、`wiki/.vitepress/cache/`、`wiki/.vitepress/dist/`（已在 `.gitignore`）。
- 手册里所有地址均为 `<服务器IP>` / `<控制台>` 占位；补充截图时**不要**把真实口令、令牌、内网地址截进去。
- 站点是纯前端静态页，不含任何 MoCenter 数据；公开访问不会暴露服务端接口。
