import { Card, Flex, Typography } from 'antd'
import type { ReactNode } from 'react'

// AuthCard 登录、注册页共用的居中卡片布局
export function AuthCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Flex align="center" justify="center" style={{ minHeight: '100vh', padding: 16 }}>
      <Card style={{ width: '100%', maxWidth: 400 }}>
        <Typography.Title level={2} style={{ textAlign: 'center', marginBottom: 4 }}>
          Liki
        </Typography.Title>
        <Typography.Paragraph type="secondary" style={{ textAlign: 'center', marginBottom: 24 }}>
          {title}
        </Typography.Paragraph>
        {children}
      </Card>
    </Flex>
  )
}
