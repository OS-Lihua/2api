/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import {
  SideDrawerSection,
  sideDrawerContentClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
  sideDrawerFooterClassName,
} from '@/components/drawer-layout'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
  SheetFooter,
} from '@/components/ui/sheet'
import { Switch } from '@/components/ui/switch'
import { CHANNEL_TYPE_OPTIONS } from '@/features/channels/constants'
import { getAllPublishers } from '@/features/models/api'

import { saveRecommendationRule, type RecommendationRule } from './api'

const invalidRule = 'Invalid recommendation rule'
const schema = z.object({
  pattern: z
    .string({ error: invalidRule })
    .trim()
    .min(1, invalidRule)
    .max(253, invalidRule),
  priority: z
    .number({ error: invalidRule })
    .int(invalidRule)
    .min(-2147483648, invalidRule)
    .max(2147483647, invalidRule),
  enabled: z.boolean({ error: invalidRule }),
  provider_name: z.string({ error: invalidRule }).max(128, invalidRule),
  provider_type: z.number({ error: invalidRule }).int(invalidRule),
  publisher_id: z.number({ error: invalidRule }).int(invalidRule),
})

export function RecommendationRuleEditor(props: {
  rule: RecommendationRule
  onClose: () => void
}) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const form = useForm({
    resolver: zodResolver(schema),
    defaultValues: props.rule,
  })
  const publishers = useQuery({
    queryKey: ['vendors', 'all-publishers'],
    queryFn: getAllPublishers,
    enabled: props.rule.kind === 'publisher',
  })
  const save = useMutation({
    mutationFn: async (values: z.infer<typeof schema>) =>
      saveRecommendationRule({ ...props.rule, ...values }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: ['recommendation-rules'] })
      await client.invalidateQueries({ queryKey: ['recommendations'] })
      props.onClose()
    },
  })
  const typeOptions = CHANNEL_TYPE_OPTIONS.map((option) => ({
    value: String(option.value),
    label: t(option.label),
  }))
  return (
    <Sheet
      open
      onOpenChange={(open) => {
        if (!open && !save.isPending) props.onClose()
      }}
    >
      <SheetContent className={sideDrawerContentClassName()}>
        <SheetHeader className={sideDrawerHeaderClassName()}>
          <SheetTitle>{t('Recommendation rules')}</SheetTitle>
          <SheetDescription>
            {t(
              'Higher priority and longer patterns rank first. Ties use the oldest rule.'
            )}
          </SheetDescription>
        </SheetHeader>
        <Form {...form}>
          <form
            id='recommendation-rule-form'
            onSubmit={form.handleSubmit((values) => save.mutate(values))}
            className={sideDrawerFormClassName()}
          >
            <fieldset disabled={save.isPending} className='contents'>
              <SideDrawerSection>
                <FormField
                  control={form.control}
                  name='pattern'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Match pattern')}</FormLabel>
                      <FormControl>
                        <Input {...field} />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <p className='text-muted-foreground text-sm'>
                  {props.rule.kind === 'provider'
                    ? t(
                        'Use a hostname or *.example.com. Wildcards match subdomains, not the apex.'
                      )
                    : t(
                        'Match model name prefixes without merging versions or quantizations.'
                      )}
                </p>
                <FormField
                  control={form.control}
                  name='priority'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Priority')}</FormLabel>
                      <FormControl>
                        <Input
                          type='number'
                          {...field}
                          onChange={(event) =>
                            field.onChange(event.target.valueAsNumber)
                          }
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                {props.rule.kind === 'provider' ? (
                  <>
                    <FormField
                      control={form.control}
                      name='provider_name'
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('Name')}</FormLabel>
                          <FormControl>
                            <Input {...field} />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                    <FormField
                      control={form.control}
                      name='provider_type'
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>{t('Type')}</FormLabel>
                          <FormControl>
                            <Combobox
                              options={typeOptions}
                              value={String(field.value)}
                              onValueChange={(value) =>
                                field.onChange(Number(value))
                              }
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  </>
                ) : (
                  <FormField
                    control={form.control}
                    name='publisher_id'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>{t('Publisher')}</FormLabel>
                        <FormControl>
                          <Combobox
                            options={(publishers.data ?? []).map(
                              (publisher) => ({
                                value: String(publisher.id),
                                label: publisher.name,
                              })
                            )}
                            value={field.value ? String(field.value) : null}
                            onValueChange={(value) =>
                              field.onChange(Number(value))
                            }
                            placeholder={t('Select publisher')}
                          />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                )}
                <FormField
                  control={form.control}
                  name='enabled'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Enabled')}</FormLabel>
                      <FormControl>
                        <Switch
                          checked={field.value}
                          onCheckedChange={field.onChange}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </SideDrawerSection>
            </fieldset>
          </form>
        </Form>
        <SheetFooter className={sideDrawerFooterClassName()}>
          <Button
            variant='outline'
            disabled={save.isPending}
            onClick={props.onClose}
          >
            {t('Close')}
          </Button>
          <Button
            type='submit'
            form='recommendation-rule-form'
            disabled={save.isPending}
          >
            {t('Save changes')}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}
