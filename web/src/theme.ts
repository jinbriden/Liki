import { theme, type ThemeConfig } from 'antd'

// 全局配色：克制的深色极简风，无霓虹 / 发光 / 渐变。
// 修改时同步 index.css 里的 --liki-* 变量（自定义布局用 CSS 变量，antd 组件用这里的 token）。
//
// 三层背景靠明度差区分区域：导航栏最暗，会话列表次之，聊天画布最亮，卡片 / 气泡 / 浮层再亮一级。
// 淡黄色强调色只用于核心操作（主按钮、发送按钮、链接）；青色 / 粉色点缀只用于在线点、导航指示条、未读数这类极小元素。
export const palette = {
  nav: '#0E1015', // 最左侧导航栏
  panel: '#14171F', // 会话列表栏
  bg: '#181B23', // 主聊天画布、详情区
  card: '#222632', // 卡片、对方消息气泡、选中的会话项、浮层
  input: '#20242F', // 悬浮输入框
  border: 'rgba(255, 255, 255, 0.06)', // 全局分割线
  text: '#ECEFF4',
  text2: '#8A93A6', // 次级文字：预览、时间、ID
  text3: '#5C6475', // 弱提示
  accent: '#F3E38A', // 主强调色（淡黄）
  accentHover: '#F7EB9F',
  onAccent: '#14171F', // 强调色上的文字
  teal: '#5EEAD4', // 点缀：在线状态、导航激活指示条
  pink: '#F472B6', // 点缀：未读数
  danger: '#e5484d',
}

export const themeConfig: ThemeConfig = {
  // darkAlgorithm 由基础色推导出整套暗色（悬停色、禁用色等），我们只覆盖少数基础 token
  algorithm: theme.darkAlgorithm,
  token: {
    colorPrimary: palette.accent,
    colorPrimaryHover: palette.accentHover,
    colorPrimaryActive: '#E6D57A',
    colorLink: palette.accent,
    colorLinkHover: palette.accentHover,
    colorError: palette.danger,
    colorBgBase: palette.bg,
    colorBgLayout: palette.bg,
    colorBgContainer: '#1C2029',
    colorBgElevated: palette.card,
    colorBorder: 'rgba(255, 255, 255, 0.10)',
    colorBorderSecondary: palette.border,
    colorTextBase: palette.text,
    borderRadius: 8,
    fontFamily:
      '-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", "Noto Sans CJK SC", sans-serif',
  },
  components: {
    // 去掉按钮底部的同色投影，保持扁平；淡黄底上用深色文字。
    // 主色在组件层再指定一次：全局的 colorPrimary 是种子色，darkAlgorithm 会从它推导出偏暗的一档给按钮用（实测 #D2C478），
    // 组件层的值不经过算法推导，按钮才是设计的 #F3E38A
    Button: {
      primaryShadow: 'none',
      defaultShadow: 'none',
      dangerShadow: 'none',
      primaryColor: palette.onAccent,
      colorPrimary: palette.accent,
      colorPrimaryHover: palette.accentHover,
      colorPrimaryActive: '#E6D57A',
    },
    // 未读数角标：粉色点缀 + 深色数字（默认是红底白字）
    Badge: { colorError: palette.pink, colorTextLightSolid: palette.onAccent },
    // 输入框聚焦只微微提亮边框，不用强调色描边和外发光
    Input: {
      activeBorderColor: 'rgba(255, 255, 255, 0.22)',
      hoverBorderColor: 'rgba(255, 255, 255, 0.14)',
      activeShadow: 'none',
    },
  },
}
