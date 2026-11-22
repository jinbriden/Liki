import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    // 和后端一样，开发阶段只监听本机
    host: '127.0.0.1',
    port: 5173,
    strictPort: true,
    // 前端请求同源的 /api/...，由 dev server 转发给 Go 服务。
    // 浏览器看到的始终是同源请求，所以后端不需要配置 CORS。
    // ws: true 让 WebSocket 握手（/api/v1/ws）也走代理；Host 和 Origin 保持一致，后端的同源检查能通过。
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080', ws: true },
    },
  },
})
