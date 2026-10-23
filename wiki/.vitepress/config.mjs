import { defineConfig } from 'vitepress'

// 发布到子路径（如 https://example.com/mocenter/）时，把 base 改成 '/mocenter/'。
export default defineConfig({
  lang: 'zh-CN',
  title: 'MoCenter 使用手册',
  description: '集中式主机远控管理系统 · 从接入到日常运维',
  base: '/',
  lastUpdated: true,
  cleanUrls: false,
  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/favicon.svg' }],
  ],

  themeConfig: {
    nav: [
      { text: '快速开始', link: '/01-快速开始' },
      { text: '功能手册', link: '/02-仪表盘与主机管理' },
      { text: '接入与运维', link: '/09-Agent接入与部署' },
      { text: '故障排查', link: '/14-故障排查FAQ' },
    ],

    sidebar: [
      {
        text: '开始',
        items: [
          { text: '概览与目录', link: '/' },
          { text: '01 快速开始', link: '/01-快速开始' },
        ],
      },
      {
        text: '功能手册',
        items: [
          { text: '02 仪表盘与主机管理', link: '/02-仪表盘与主机管理' },
          { text: '03 远程终端', link: '/03-远程终端' },
          { text: '04 文件管理', link: '/04-文件管理' },
          { text: '05 远程桌面（VNC）', link: '/05-远程桌面VNC' },
          { text: '06 端口映射', link: '/06-端口映射' },
          { text: '07 运维操作', link: '/07-运维操作' },
          { text: '08 延迟监测', link: '/08-延迟监测' },
        ],
      },
      {
        text: '接入与运维',
        items: [
          { text: '09 Agent 接入与部署', link: '/09-Agent接入与部署' },
          { text: '10 系统配置与安全', link: '/10-系统配置与安全' },
          { text: '11 展示配置与公开页', link: '/11-展示配置与公开页' },
          { text: '12 备份恢复与审计', link: '/12-备份恢复与审计' },
        ],
      },
      {
        text: '附录',
        items: [
          { text: '13 参数与配置速查', link: '/13-参数与配置速查' },
          { text: '14 故障排查 FAQ', link: '/14-故障排查FAQ' },
        ],
      },
    ],

    search: {
      provider: 'local',
      options: {
        translations: {
          button: { buttonText: '搜索文档', buttonAriaLabel: '搜索文档' },
          modal: {
            noResultsText: '没有找到相关内容',
            resetButtonTitle: '清除条件',
            footer: { selectText: '选择', navigateText: '切换', closeText: '关闭' },
          },
        },
      },
    },

    outline: { level: [2, 3], label: '本页目录' },
    docFooter: { prev: '上一篇', next: '下一篇' },
    returnToTopLabel: '回到顶部',
    sidebarMenuLabel: '目录',
    darkModeSwitchLabel: '主题',
    lastUpdatedText: '最后更新',
    footer: {
      message: 'MoCenter · 集中式主机远控管理系统',
      copyright: '使用手册',
    },
  },
})
