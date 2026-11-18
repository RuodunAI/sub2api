export default {
  title: '插件联网',
  description: '插件经出口隧道可以访问哪些域名，对所有插件生效。插件仍须持有 net 权限，设为“仅白名单”的插件仍只能访问它获批的域名。',
  defaultLabel: '所有域名的默认设置',
  defaultHint: '选择插件是否默认可以访问所有域名',
  modes: {
    allow_all: '允许所有域名',
    block_all: '屏蔽所有域名'
  },
  info: {
    allow_all: '插件可以访问所有域名，但您在下方屏蔽的域名除外。',
    block_all: '插件只能访问您在下方允许的域名。'
  },
  list: {
    allow_all: '已屏蔽的域名',
    block_all: '已允许的域名'
  },
  listHint: {
    allow_all: '插件无法访问这些域名及其子域名。',
    block_all: '插件可以访问这些域名及其子域名。'
  },
  domain: '域名',
  empty: '尚未添加任何域名。',
  add: '添加域名',
  egressTabHint: '所有插件共用的可访问域名在“系统设置 → 插件联网”里设置，与这里的出口策略同时生效。',
  egressTabLink: '前往插件联网',
  dialog: {
    title: {
      allow_all: '添加要屏蔽的域名',
      block_all: '添加要允许的域名'
    },
    addRow: '添加其他域名',
    bulk: '批量添加',
    single: '逐个添加',
    bulkHint: '每行一个域名，空行会被忽略。',
    continue: '继续',
    invalid: '不是有效的域名或 IP 地址（不带 https://、端口和路径）',
    lineInvalid: '第 {line} 行“{value}”不是有效的域名或 IP 地址',
    empty: '请至少填写一个域名。'
  }
}
