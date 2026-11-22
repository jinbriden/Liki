import { theme, type ThemeConfig } from 'antd'

// 全局配色：黑色系，克制、无霓虹 / 发光 / 渐变。
// 修改时同步 index.css 里的 --liki-* 变量（自定义布局用 CSS 变量，antd 组件用这里的 token）。
export const palette = {
  bg: '#0e0e10', // 页面底色
  panel: '#151517', // 侧边栏、列表区
  elevated: '#1f1f23', // 弹窗、浮层
  border: '#26262b',
  text: '#e8e8ea',
  primary: '#4f7cff', // 低饱和蓝，只用于主按钮、角标、链接
  danger: '#e5484d',
  bubbleMine: '#2e2f36', // 自己发的消息气泡（只在 index.css 里使用）
}

export const themeConfig: ThemeConfig = {
  // darkAlgorithm 由基础色推导出整套暗色（悬停色、禁用色等），我们只覆盖少数基础 token
  algorithm: theme.darkAlgorithm,
  token: {
    colorPrimary: palette.primary,
    colorError: palette.danger,
    colorBgBase: palette.bg,
    colorBgLayout: palette.bg,
    colorBgContainer: palette.panel,
    colorBgElevated: palette.elevated,
    colorBorder: '#303036',
    colorBorderSecondary: palette.border,
    colorTextBase: palette.text,
    borderRadius: 6,
    fontFamily:
      '-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", "Noto Sans CJK SC", sans-serif',
  },
  components: {
    // 去掉按钮底部的同色投影，保持扁平
    Button: { primaryShadow: 'none', defaultShadow: 'none', dangerShadow: 'none' },
  },
}
